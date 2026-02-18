# stack-update

Updates an existing CloudFormation stack using a provided template.
Creates a change set, waits for user confirmation, and then executes the change set.

Examples:

Update stack “my-service” from a template having the same name:

```
stack-update dir/my-service.yml
```

Update stack “my-service” from a template:

```
stack-update -n my-service path/to/cloudformation.yml
```

Update a stack from a template while overriding (or adding) stack parameter(s):

```
stack-update my-service.yml Version=v123
```

Permissions required:

- `cloudformation:DescribeStacks`
- `cloudformation:CreateChangeSet`
- `cloudformation:DeleteChangeSet`
- `cloudformation:DescribeChangeSet`
- `cloudformation:ExecuteChangeSet`
- `cloudformation:DescribeStackEvents`

When template size exceeds 51,200 bytes:

- `s3:ListAllMyBuckets`
- `s3:PutObject`

Note that this tool does not cover every possible use case.
You may still occasionally need to fall back to the CloudFormation console or other tools.

To install:

```
go install github.com/artyom/stack-update@latest
```
