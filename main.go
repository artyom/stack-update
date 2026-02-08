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
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	log.SetFlags(0)
	var name string
	flag.StringVar(&name, "n", name, "stack `name`; if not set, derived from template name")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, name, flag.Arg(0)); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, stackName, templateFile string) error {
	if templateFile == "" {
		return errors.New("want template file as the first argument")
	}
	if stackName == "" {
		s := filepath.Base(templateFile)
		stackName = strings.TrimSuffix(s, filepath.Ext(s))
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

	desc, err := svc.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: &stackName})
	if err != nil {
		return err
	}
	if l := len(desc.Stacks); l != 1 {
		return fmt.Errorf("DescribeStacks returned %d stacks, expected 1", l)
	}
	stack := desc.Stacks[0]
	var params []types.Parameter
	for _, p := range stack.Parameters {
		k := unptr(p.ParameterKey)
		params = append(params, types.Parameter{ParameterKey: &k, UsePreviousValue: new(true)})
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
		// TODO: corner case — when the change itself creates a resource that requires new capability
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

	log.Print("waiting until change set is ready")

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

	if len(descOut.Changes) != 0 {
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "Action\tReplacement\tResType\tLogicalID\tPhysicalID\t")
		for _, c := range descOut.Changes {
			if c.Type != types.ChangeTypeResource {
				return fmt.Errorf("unsupported change type: %v", c.Type)
			}
			rc := c.ResourceChange
			fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%v\t\n", rc.Action, rc.Replacement, unptr(rc.ResourceType), unptr(rc.LogicalResourceId), unptr(rc.PhysicalResourceId))
		}
		tw.Flush()
	}

	fmt.Println()
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

	log.Print("waiting for update to complete, follow the stack update progress in the AWS console")
	if err := openConsole(*stack.StackId); err != nil {
		log.Printf("opening browser: %v", err)
	}

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
			return fmt.Errorf("change set execution status: %v", descOut.ExecutionStatus)
		}
	}
	skipChangeSetDelete = true
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

func openConsole(arn string) error {
	region, err := arnRegion(arn)
	if err != nil {
		return err
	}
	u := url.URL{
		Scheme:   "https",
		Host:     region + ".console.aws.amazon.com",
		Path:     "/go/view",
		RawQuery: (url.Values{"arn": {arn}}).Encode(),
	}
	var openCmd string
	switch runtime.GOOS {
	case "darwin":
		openCmd = "open"
	case "linux", "freebsd":
		openCmd = "xdg-open"
	case "windows":
		openCmd = "explorer.exe"
	default:
		return fmt.Errorf("don't know how to open url on %s", runtime.GOOS)
	}
	return exec.Command(openCmd, u.String()).Run()
}

func arnRegion(arn string) (string, error) {
	if !strings.HasPrefix(arn, "arn:") {
		return "", fmt.Errorf("%q does not look like arn", arn)
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
		return "", fmt.Errorf("cannot extract region from arn %q", arn)
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
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [flags] path/to/template.yml\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
}
