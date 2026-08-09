package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	log.SetFlags(0)
	var name string
	var express bool
	flag.StringVar(&name, "n", name, "stack `name`; if not set, derived from template name")
	flag.BoolVar(&express, "e", express, "express mode")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	rest := flag.Args()
	if len(rest) >= 1 {
		rest = rest[1:]
	}
	if err := run(ctx, express, name, flag.Arg(0), rest); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, express bool, stackName, templateFile string, rest []string) error {
	if templateFile == "" {
		return errors.New("expected template file path as the first argument")
	}
	if stackName == "" {
		s := filepath.Base(templateFile)
		stackName = strings.TrimSuffix(s, filepath.Ext(s))
	}
	overrides, err := parameterOverrides(rest)
	if err != nil {
		return err
	}

	template, err := os.ReadFile(templateFile)
	if err != nil {
		return err
	}
	if len(template) > 1<<20 {
		return errors.New("template is too big")
	}

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	svc := cloudformation.NewFromConfig(cfg)

	if filepath.Base(os.Args[0]) == "stack-create" {
		return createStack(ctx, cfg, svc, template, stackName, overrides)
	}

	desc, err := svc.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: &stackName})
	if err != nil {
		return err
	}
	if l := len(desc.Stacks); l != 1 {
		return fmt.Errorf("DescribeStacks returned %d stacks, expected 1", l)
	}
	stack := desc.Stacks[0]
	if err := allowUpdates(filepath.Dir(templateFile), unptr(stack.StackId)); err != nil {
		return err
	}
	var params []types.Parameter
	for _, p := range stack.Parameters {
		k := unptr(p.ParameterKey)
		if v, ok := overrides[k]; ok {
			params = append(params, types.Parameter{ParameterKey: &k, ParameterValue: &v})
			delete(overrides, k)
			continue
		}
		if !bytes.Contains(template, []byte(k)) { // not super presice, but better than nothing
			continue
		}
		params = append(params, types.Parameter{ParameterKey: &k, UsePreviousValue: new(true)})
	}
	if len(overrides) != 0 {
		log.Printf("stack has no parameters with these names (it's ok if your template adds them): %s", strings.Join(slices.Sorted(maps.Keys(overrides)), ", "))
		for k, v := range overrides {
			params = append(params, types.Parameter{ParameterKey: &k, ParameterValue: &v})
		}
	}

	changeSetID := "cs-" + rand.Text()
	inp := &cloudformation.CreateChangeSetInput{
		StackName:     &stackName,
		ChangeSetName: &changeSetID,
		ChangeSetType: types.ChangeSetTypeUpdate,
		Parameters:    params,
		TemplateBody:  new(string(template)),
		Description:   new("created using stack-update tool"),
		Capabilities:  stack.Capabilities,
	}
	if express {
		inp.DeploymentConfig = &types.DeploymentConfig{Mode: types.DeploymentConfigModeExpress, DisableRollback: new(false)}
	}

	// Even though there's a logic below on CreateChangeSet that catches types.InsufficientCapabilitiesException,
	// CloudFormation isn't very consistent returning it, and in some cases I've seen CreateChangeSet succeeding,
	// and failing on the execution stage, reaching FAILED status and “Requires capabilities : [CAPABILITY_IAM]”
	// status reason.
	if regexp.MustCompile(`Type"?\s*:\s*"?AWS::IAM::`).Match(template) {
		for _, cap := range [...]types.Capability{types.CapabilityCapabilityIam, types.CapabilityCapabilityNamedIam} {
			if !slices.Contains(inp.Capabilities, cap) {
				inp.Capabilities = append(inp.Capabilities, cap)
				log.Println("added capability", cap)
			}
		}
	}

	if len(template) > 51_200 { // template is too big to be provided inline
		region, err := arnRegion(*stack.StackId)
		if err != nil {
			return err
		}
		url, err := uploadTemplate(ctx, s3.NewFromConfig(cfg), region, stackName, template)
		if err != nil {
			return fmt.Errorf("uploading template: %w", err)
		}
		inp.TemplateBody = nil
		inp.TemplateURL = &url
	}

	createOut, err := svc.CreateChangeSet(ctx, inp)
	if e, ok := errors.AsType[*types.InsufficientCapabilitiesException](err); ok {
		errtext := e.Error()
		var updatedCaps bool
		for _, s := range (types.Capability)("").Values() {
			if strings.Contains(errtext, string(s)) && !slices.Contains(inp.Capabilities, s) {
				log.Println("added missing capability", s)
				inp.Capabilities = append(inp.Capabilities, s)
				updatedCaps = true
			}
		}
		if updatedCaps {
			createOut, err = svc.CreateChangeSet(ctx, inp)
		}
	}
	if err != nil {
		return fmt.Errorf("CreateChangeSet: %w", err)
	}

	var skipChangeSetDelete bool
	defer func() {
		if skipChangeSetDelete {
			return
		}
		// don't use outer scope ctx because it may be already canceled
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := svc.DeleteChangeSet(ctx, &cloudformation.DeleteChangeSetInput{
			StackName:     &stackName,
			ChangeSetName: &changeSetID,
		}); err != nil {
			log.Printf("change set %q delete: %v", changeSetID, err)
		}
	}()

	log.Print("waiting for change set")

	var descOut *cloudformation.DescribeChangeSetOutput

createWaitLoop:
	for ticker := time.NewTicker(3 * time.Second); ; {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		descOut, err = svc.DescribeChangeSet(ctx, &cloudformation.DescribeChangeSetInput{ChangeSetName: createOut.Id})
		if err != nil {
			return fmt.Errorf("DescribeChangeSet: %w", err)
		}

		switch descOut.Status {
		case types.ChangeSetStatusCreatePending, types.ChangeSetStatusCreateInProgress: // continue polling
		case types.ChangeSetStatusCreateComplete:
			break createWaitLoop
		case types.ChangeSetStatusFailed:
			if descOut.StatusReason != nil && *descOut.StatusReason != "" {
				if strings.Contains(*descOut.StatusReason, "DescribeEvents") {
					if err := logChangeSetFailedEvents(ctx, svc, *createOut.Id); err != nil {
						log.Printf("DescribeEvents: %v", err)
					}
				}
				return fmt.Errorf("change set create: %v, %s", descOut.Status, *descOut.StatusReason)
			}
			return fmt.Errorf("change set create: %v", descOut.Status)
		default:
			return fmt.Errorf("unexpected change set status: %v", descOut.Status)
		}
	}

	if s := descOut.ExecutionStatus; s != types.ExecutionStatusAvailable {
		return fmt.Errorf("unexpected change set execution status: %v", s)
	}

	var warn bool
	if len(descOut.Changes) != 0 {
		tbl := table{{"Action", "Replacement", "ResType", "LogicalID", "PhysicalID"}}
		for _, c := range descOut.Changes {
			if c.Type != types.ChangeTypeResource {
				return fmt.Errorf("unsupported change type: %v", c.Type)
			}
			rc := c.ResourceChange
			row := []any{rc.Action, rc.Replacement, unptr(rc.ResourceType), unptr(rc.LogicalResourceId), styled{v: unptr(rc.PhysicalResourceId), sgr: sgrDim}}
			if rc.Action == types.ChangeActionRemove {
				row[0] = styled{v: row[0], sgr: sgrBold}
			}
			if rc.Replacement != "" && rc.Replacement != types.ReplacementFalse {
				row[1] = styled{v: row[1], sgr: sgrBold}
			}
			tbl = append(tbl, row)
			warn = warn || rc.Action == types.ChangeActionRemove || (rc.Replacement != "" && rc.Replacement != types.ReplacementFalse)
		}
		fmt.Println()
		fmt.Print(tbl.Render())
	}

	fmt.Println()
	if warn {
		fmt.Println("\033[1mWarning: resources may be replaced or removed.\033[0m")
	}
	fmt.Print("Do you want to continue? [y/N] ")
	input, err := bufio.NewReader(io.LimitReader(os.Stdin, 10)).ReadString('\n')
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "y", "yes":
	default:
		return errors.New("aborted")
	}

	if _, err := svc.ExecuteChangeSet(ctx, &cloudformation.ExecuteChangeSetInput{ChangeSetName: createOut.Id}); err != nil {
		return fmt.Errorf("ExecuteChangeSet: %w", err)
	}

	log.Print("waiting for update to complete")
	openBrowser := sync.OnceFunc(func() {
		if ok, _ := strconv.ParseBool(os.Getenv("STACK_UPDATE_NO_BROWSER")); !ok {
			if err := openConsole(*stack.StackId); err != nil {
				log.Printf("opening browser: %v", err)
			}
		}
	})
	defer time.AfterFunc(3*time.Minute, openBrowser).Stop()

executeWaitLoop:
	for ticker := time.NewTicker(3 * time.Second); ; {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		descOut, err = svc.DescribeChangeSet(ctx, &cloudformation.DescribeChangeSetInput{ChangeSetName: createOut.Id})
		if err != nil {
			return fmt.Errorf("DescribeChangeSet: %w", err)
		}
		switch descOut.ExecutionStatus {
		case types.ExecutionStatusExecuteInProgress:
		case types.ExecutionStatusExecuteComplete:
			break executeWaitLoop
		default:
			openBrowser()
			return fmt.Errorf("change set execution status: %v", descOut.ExecutionStatus)
		}
	}
	skipChangeSetDelete = true
	return nil
}

func createStack(ctx context.Context, cfg aws.Config, svc *cloudformation.Client, template []byte, stackName string, overrides map[string]string) error {
	var params []types.Parameter
	for k, v := range overrides {
		params = append(params, types.Parameter{ParameterKey: &k, ParameterValue: &v})
	}
	inp := &cloudformation.CreateStackInput{
		StackName:    &stackName,
		Parameters:   params,
		TemplateBody: new(string(template)),
		OnFailure:    types.OnFailureDelete,
	}
	// TODO: consolidate with similar logic within the run function
	if regexp.MustCompile(`Type"?\s*:\s*"?AWS::IAM::`).Match(template) {
		inp.Capabilities = append(inp.Capabilities, types.CapabilityCapabilityIam, types.CapabilityCapabilityNamedIam)
		log.Println("capabilities added:", inp.Capabilities)
	}
	region := svc.Options().Region
	if region == "" {
		return errors.New("FIXME: region is not set")
	}
	if len(template) > 51_200 { // template is too big to be provided inline
		url, err := uploadTemplate(ctx, s3.NewFromConfig(cfg), region, stackName, template)
		if err != nil {
			return fmt.Errorf("uploading template: %w", err)
		}
		inp.TemplateBody = nil
		inp.TemplateURL = &url
	}
	res, err := svc.CreateStack(ctx, inp)
	if err != nil {
		return err
	}
	log.Print(*res.StackId)
	if err := openConsole(*res.StackId); err != nil {
		log.Printf("opening browser: %v", err)
	}
	return nil
}

func uploadTemplate(ctx context.Context, svc *s3.Client, region, stackName string, body []byte) (string, error) {
	p := s3.NewListBucketsPaginator(svc, &s3.ListBucketsInput{
		Prefix:       new("cf-templates-"),
		BucketRegion: &region,
	})
	var bucket string
	suffix := "-" + region
paginate:
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return "", err
		}
		for _, b := range page.Buckets {
			if strings.HasSuffix(*b.Name, suffix) {
				bucket = *b.Name
				break paginate
			}
		}
	}
	if bucket == "" {
		return "", errors.New("cannot discover bucket to upload template to")
	}
	key := path.Join(stackName, fmt.Sprintf("%x", sha256.Sum256(body)))
	if _, err := svc.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   bytes.NewReader(body),
	}); err != nil {
		return "", err
	}
	return (&url.URL{
		Scheme: "https",
		Host:   "s3." + region + ".amazonaws.com",
		Path:   path.Join(bucket, key),
	}).String(), nil
}

func logChangeSetFailedEvents(ctx context.Context, svc *cloudformation.Client, changeSetName string) error {
	p := cloudformation.NewDescribeEventsPaginator(svc, &cloudformation.DescribeEventsInput{
		ChangeSetName: &changeSetName,
		Filters:       &types.EventFilter{FailedEvents: new(true)},
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, e := range page.OperationEvents {
			log.Println(unptr(e.LogicalResourceId), e.EventType, unptr(e.ValidationName), e.ValidationStatus, unptr(e.ValidationPath), unptr(e.ValidationStatusReason))
		}
	}
	return nil
}

func parameterOverrides(args []string) (map[string]string, error) {
	var m map[string]string
	for _, s := range args {
		if m == nil {
			m = map[string]string{}
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("want key=value pair for stack parameter, got %q", s)
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("want key=value pair for stack parameter where both key is non-empty, got %q", s)
		}
		m[k] = v
	}
	return m, nil
}

func openConsole(arn string) error {
	u := url.URL{
		Scheme:   "https",
		Host:     "console.aws.amazon.com",
		Path:     "/go/view",
		RawQuery: (url.Values{"arn": {arn}}).Encode(),
	}
	var openCmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		openCmd = "open"
		args = []string{"-g"}
	case "linux", "freebsd":
		openCmd = "xdg-open"
	case "windows":
		openCmd = "explorer.exe"
	default:
		return fmt.Errorf("don't know how to open an URL on %s", runtime.GOOS)
	}
	return exec.Command(openCmd, append(args, u.String())...).Run()
}

func arnRegion(arn string) (string, error) {
	if !strings.HasPrefix(arn, "arn:") {
		return "", fmt.Errorf("%q does not look like an ARN", arn)
	}
	var region string
	var i int
	for s := range strings.SplitSeq(arn, ":") {
		if i == 3 {
			region = s
			break
		}
		i++
	}
	if region == "" {
		return "", fmt.Errorf("cannot extract region from ARN %q", arn)
	}
	return region, nil
}

func unptr[T any](v *T) T {
	var zero T
	if v != nil {
		return *v
	}
	return zero
}

func init() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [flags] path/to/template.yml [key=value ...]\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
}

type table [][]any

type styled struct {
	v   any
	sgr byte
}

const sgrBold = 1
const sgrDim = 2

func (t table) Render() string {
	ws := make([]int, len(t[0]))
	hasValues := make([]bool, len(t[0]))
	var tmp []byte
	for ri, row := range t {
		for i, v := range row {
			tmp = tmp[:0]
			switch v := v.(type) {
			case styled:
				tmp = fmt.Append(tmp, v.v)
			default:
				tmp = fmt.Append(tmp, v)
			}
			ws[i] = max(ws[i], len(tmp))
			if ri != 0 {
				hasValues[i] = hasValues[i] || len(tmp) != 0
			}
		}
	}
	formats := make([]string, len(t[0]))
	for i, w := range ws {
		formats[i] = fmt.Sprintf("%%-%dv", w)
	}
	for i := len(hasValues) - 1; i >= 0; i-- {
		if hasValues[i] {
			formats[i] = "%v"
			break
		}
	}
	var out []byte
	for _, row := range t {
		var needSep bool
		for i, v := range row {
			if !hasValues[i] {
				continue
			}
			if needSep {
				out = append(out, "  "...)
			}
			switch v := v.(type) {
			case styled:
				out = append(out, "\033["...)
				out = strconv.AppendUint(out, uint64(v.sgr), 10)
				out = append(out, 'm')
				out = fmt.Appendf(out, formats[i], v.v)
				out = append(out, "\033[0m"...)
			default:
				out = fmt.Appendf(out, formats[i], v)
			}
			needSep = true
		}
		out = append(out, '\n')
	}
	return string(out)
}

func allowUpdates(dir, arn string) error {
	var targetAccount string
	var i int
	for s := range strings.SplitSeq(arn, ":") {
		if i == 4 { // arn:aws:cloudformation:us-east-1:0123456789:stack/name/random
			targetAccount = s
			break
		}
		i++
	}
	if targetAccount == "" {
		return fmt.Errorf("cannot extract account id from stack id %q", arn)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".stack-update"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for line := range strings.Lines(string(data)) {
		if after, ok := strings.CutPrefix(line, "only:"); ok {
			for s := range strings.SplitSeq(after, ",") {
				s = strings.TrimSpace(s)
				if targetAccount == s {
					return nil
				}
			}
			return fmt.Errorf("updating stack on account %s is not allowed by .stack-update policy", targetAccount)
		}
	}
	return nil
}
