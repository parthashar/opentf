// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package s3

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	awsbase "github.com/hashicorp/aws-sdk-go-base/v2"
	"github.com/opentofu/opentofu/internal/backend"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/httpclient"
	"github.com/opentofu/opentofu/internal/logging"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/version"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/gocty"
)

func New() backend.Backend {
	return &Backend{}
}

type Backend struct {
	s3Client  *s3.Client
	dynClient *dynamodb.Client
	awsConfig aws.Config

	bucketName            string
	keyName               string
	serverSideEncryption  bool
	customerEncryptionKey []byte
	acl                   string
	kmsKeyID              string
	ddbTable              string
	workspaceKeyPrefix    string
}

// ConfigSchema returns a description of the expected configuration
// structure for the receiving backend.
func (b *Backend) ConfigSchema() *configschema.Block {
	return &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"bucket": {
				Type:        cty.String,
				Required:    true,
				Description: "The name of the S3 bucket",
			},
			"key": {
				Type:        cty.String,
				Required:    true,
				Description: "The path to the state file inside the bucket",
			},
			"region": {
				Type:        cty.String,
				Optional:    true,
				Description: "AWS region of the S3 Bucket and DynamoDB Table (if used).",
			},
			"dynamodb_endpoint": {
				Type:        cty.String,
				Optional:    true,
				Description: "A custom endpoint for the DynamoDB API",
			},
			"endpoint": {
				Type:        cty.String,
				Optional:    true,
				Description: "A custom endpoint for the S3 API",
			},
			"iam_endpoint": {
				Type:        cty.String,
				Optional:    true,
				Description: "A custom endpoint for the IAM API",
			},
			"sts_endpoint": {
				Type:        cty.String,
				Optional:    true,
				Description: "A custom endpoint for the STS API",
			},
			"encrypt": {
				Type:        cty.Bool,
				Optional:    true,
				Description: "Whether to enable server side encryption of the state file",
			},
			"acl": {
				Type:        cty.String,
				Optional:    true,
				Description: "Canned ACL to be applied to the state file",
			},
			"access_key": {
				Type:        cty.String,
				Optional:    true,
				Description: "AWS access key",
			},
			"secret_key": {
				Type:        cty.String,
				Optional:    true,
				Description: "AWS secret key",
			},
			"kms_key_id": {
				Type:        cty.String,
				Optional:    true,
				Description: "The ARN of a KMS Key to use for encrypting the state",
			},
			"dynamodb_table": {
				Type:        cty.String,
				Optional:    true,
				Description: "DynamoDB table for state locking and consistency",
			},
			"profile": {
				Type:        cty.String,
				Optional:    true,
				Description: "AWS profile name",
			},
			"shared_credentials_file": {
				Type:        cty.String,
				Optional:    true,
				Description: "Path to a shared credentials file",
			},
			"shared_credentials_files": {
				Type:        cty.Set(cty.String),
				Optional:    true,
				Description: "Paths to a shared credentials files",
			},
			"shared_config_files": {
				Type:        cty.Set(cty.String),
				Optional:    true,
				Description: "Paths to shared config files",
			},
			"token": {
				Type:        cty.String,
				Optional:    true,
				Description: "MFA token",
			},
			"skip_credentials_validation": {
				Type:        cty.Bool,
				Optional:    true,
				Description: "Skip the credentials validation via STS API.",
			},
			"skip_metadata_api_check": {
				Type:        cty.Bool,
				Optional:    true,
				Description: "Skip the AWS Metadata API check.",
			},
			"skip_region_validation": {
				Type:        cty.Bool,
				Optional:    true,
				Description: "Skip static validation of region name.",
			},
			"sse_customer_key": {
				Type:        cty.String,
				Optional:    true,
				Description: "The base64-encoded encryption key to use for server-side encryption with customer-provided keys (SSE-C).",
				Sensitive:   true,
			},
			"role_arn": {
				Type:        cty.String,
				Optional:    true,
				Description: "The role to be assumed",
				Deprecated:  true,
			},
			"session_name": {
				Type:        cty.String,
				Optional:    true,
				Description: "The session name to use when assuming the role.",
				Deprecated:  true,
			},
			"external_id": {
				Type:        cty.String,
				Optional:    true,
				Description: "The external ID to use when assuming the role",
				Deprecated:  true,
			},

			"assume_role_duration_seconds": {
				Type:        cty.Number,
				Optional:    true,
				Description: "Seconds to restrict the assume role session duration.",
				Deprecated:  true,
			},

			"assume_role_policy": {
				Type:        cty.String,
				Optional:    true,
				Description: "IAM Policy JSON describing further restricting permissions for the IAM Role being assumed.",
				Deprecated:  true,
			},

			"assume_role_policy_arns": {
				Type:        cty.Set(cty.String),
				Optional:    true,
				Description: "Amazon Resource Names (ARNs) of IAM Policies describing further restricting permissions for the IAM Role being assumed.",
				Deprecated:  true,
			},

			"assume_role_tags": {
				Type:        cty.Map(cty.String),
				Optional:    true,
				Description: "Assume role session tags.",
				Deprecated:  true,
			},

			"assume_role_transitive_tag_keys": {
				Type:        cty.Set(cty.String),
				Optional:    true,
				Description: "Assume role session tag keys to pass to any subsequent sessions.",
				Deprecated:  true,
			},

			"workspace_key_prefix": {
				Type:        cty.String,
				Optional:    true,
				Description: "The prefix applied to the non-default state path inside the bucket.",
			},

			"force_path_style": {
				Type:        cty.Bool,
				Optional:    true,
				Description: "Force s3 to use path style api.",
			},

			"max_retries": {
				Type:        cty.Number,
				Optional:    true,
				Description: "The maximum number of times an AWS API request is retried on retryable failure.",
			},
			"use_legacy_workflow": {
				Type:        cty.Bool,
				Optional:    true,
				Description: "Use the legacy authentication workflow, preferring environment variables over backend configuration.",
			},
			"assume_role": {
				NestedType: &configschema.Object{
					Nesting: configschema.NestingSingle,
					Attributes: map[string]*configschema.Attribute{
						"role_arn": {
							Type:        cty.String,
							Required:    true,
							Description: "The role to be assumed.",
						},
						"duration": {
							Type:        cty.String,
							Optional:    true,
							Description: "Seconds to restrict the assume role session duration.",
						},
						"external_id": {
							Type:        cty.String,
							Optional:    true,
							Description: "The external ID to use when assuming the role",
						},
						"policy": {
							Type:        cty.String,
							Optional:    true,
							Description: "IAM Policy JSON describing further restricting permissions for the IAM Role being assumed.",
						},
						"policy_arns": {
							Type:        cty.Set(cty.String),
							Optional:    true,
							Description: "Amazon Resource Names (ARNs) of IAM Policies describing further restricting permissions for the IAM Role being assumed.",
						},
						"session_name": {
							Type:        cty.String,
							Optional:    true,
							Description: "The session name to use when assuming the role.",
						},
						"tags": {
							Type:        cty.Map(cty.String),
							Optional:    true,
							Description: "Assume role session tags.",
						},
						"transitive_tag_keys": {
							Type:        cty.Set(cty.String),
							Optional:    true,
							Description: "Assume role session tag keys to pass to any subsequent sessions.",
						},
						//
						// NOT SUPPORTED by `aws-sdk-go-base/v1`
						// Cannot be added yet.
						//
						// "source_identity": stringAttribute{
						// 	configschema.Attribute{
						// 		Type:         cty.String,
						// 		Optional:     true,
						// 		Description:  "Source identity specified by the principal assuming the role.",
						// 		ValidateFunc: validAssumeRoleSourceIdentity,
						// 	},
						// },
					},
				},
			},
			"forbidden_account_ids": {
				Type:        cty.Set(cty.String),
				Optional:    true,
				Description: "List of forbidden AWS account IDs.",
			},
			"allowed_account_ids": {
				Type:        cty.Set(cty.String),
				Optional:    true,
				Description: "List of allowed AWS account IDs.",
			},
		},
	}
}

// PrepareConfig checks the validity of the values in the given
// configuration, and inserts any missing defaults, assuming that its
// structure has already been validated per the schema returned by
// ConfigSchema.
func (b *Backend) PrepareConfig(obj cty.Value) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	if obj.IsNull() {
		return obj, diags
	}

	if val := obj.GetAttr("bucket"); val.IsNull() || val.AsString() == "" {
		diags = diags.Append(tfdiags.AttributeValue(
			tfdiags.Error,
			"Invalid bucket value",
			`The "bucket" attribute value must not be empty.`,
			cty.Path{cty.GetAttrStep{Name: "bucket"}},
		))
	}

	if val := obj.GetAttr("key"); val.IsNull() || val.AsString() == "" {
		diags = diags.Append(tfdiags.AttributeValue(
			tfdiags.Error,
			"Invalid key value",
			`The "key" attribute value must not be empty.`,
			cty.Path{cty.GetAttrStep{Name: "key"}},
		))
	} else if strings.HasPrefix(val.AsString(), "/") || strings.HasSuffix(val.AsString(), "/") {
		// S3 will strip leading slashes from an object, so while this will
		// technically be accepted by S3, it will break our workspace hierarchy.
		// S3 will recognize objects with a trailing slash as a directory
		// so they should not be valid keys
		diags = diags.Append(tfdiags.AttributeValue(
			tfdiags.Error,
			"Invalid key value",
			`The "key" attribute value must not start or end with with "/".`,
			cty.Path{cty.GetAttrStep{Name: "key"}},
		))
	}

	if val := obj.GetAttr("region"); val.IsNull() || val.AsString() == "" {
		if os.Getenv("AWS_REGION") == "" && os.Getenv("AWS_DEFAULT_REGION") == "" {
			diags = diags.Append(tfdiags.AttributeValue(
				tfdiags.Error,
				"Missing region value",
				`The "region" attribute or the "AWS_REGION" or "AWS_DEFAULT_REGION" environment variables must be set.`,
				cty.Path{cty.GetAttrStep{Name: "region"}},
			))
		}
	}

	if val := obj.GetAttr("kms_key_id"); !val.IsNull() && val.AsString() != "" {
		if val := obj.GetAttr("sse_customer_key"); !val.IsNull() && val.AsString() != "" {
			diags = diags.Append(tfdiags.AttributeValue(
				tfdiags.Error,
				"Invalid encryption configuration",
				encryptionKeyConflictError,
				cty.Path{},
			))
		} else if customerKey := os.Getenv("AWS_SSE_CUSTOMER_KEY"); customerKey != "" {
			diags = diags.Append(tfdiags.AttributeValue(
				tfdiags.Error,
				"Invalid encryption configuration",
				encryptionKeyConflictEnvVarError,
				cty.Path{},
			))
		}

		diags = diags.Append(validateKMSKey(cty.Path{cty.GetAttrStep{Name: "kms_key_id"}}, val.AsString()))
	}

	if val := obj.GetAttr("workspace_key_prefix"); !val.IsNull() {
		if v := val.AsString(); strings.HasPrefix(v, "/") || strings.HasSuffix(v, "/") {
			diags = diags.Append(tfdiags.AttributeValue(
				tfdiags.Error,
				"Invalid workspace_key_prefix value",
				`The "workspace_key_prefix" attribute value must not start with "/".`,
				cty.Path{cty.GetAttrStep{Name: "workspace_key_prefix"}},
			))
		}
	}

	validateAttributesConflict(
		cty.GetAttrPath("shared_credentials_file"),
		cty.GetAttrPath("shared_credentials_files"),
	)(obj, cty.Path{}, &diags)

	attrPath := cty.GetAttrPath("shared_credentials_file")
	if val := obj.GetAttr("shared_credentials_file"); !val.IsNull() {
		detail := fmt.Sprintf(
			`Parameter "%s" is deprecated. Use "%s" instead.`,
			pathString(attrPath),
			pathString(cty.GetAttrPath("shared_credentials_files")))

		diags = diags.Append(attributeWarningDiag(
			"Deprecated Parameter",
			detail,
			attrPath))
	}

	var assumeRoleDeprecatedFields = map[string]string{
		"role_arn":                        "assume_role.role_arn",
		"session_name":                    "assume_role.session_name",
		"external_id":                     "assume_role.external_id",
		"assume_role_duration_seconds":    "assume_role.duration",
		"assume_role_policy":              "assume_role.policy",
		"assume_role_policy_arns":         "assume_role.policy_arns",
		"assume_role_tags":                "assume_role.tags",
		"assume_role_transitive_tag_keys": "assume_role.transitive_tag_keys",
	}

	if val := obj.GetAttr("assume_role"); !val.IsNull() {
		diags = diags.Append(validateNestedAssumeRole(val, cty.Path{cty.GetAttrStep{Name: "assume_role"}}))

		if defined := findDeprecatedFields(obj, assumeRoleDeprecatedFields); len(defined) != 0 {
			diags = diags.Append(tfdiags.WholeContainingBody(
				tfdiags.Error,
				"Conflicting Parameters",
				`The following deprecated parameters conflict with the parameter "assume_role". Replace them as follows:`+"\n"+
					formatDeprecated(defined),
			))
		}
	} else {
		if defined := findDeprecatedFields(obj, assumeRoleDeprecatedFields); len(defined) != 0 {
			diags = diags.Append(tfdiags.WholeContainingBody(
				tfdiags.Warning,
				"Deprecated Parameters",
				`The following parameters have been deprecated. Replace them as follows:`+"\n"+
					formatDeprecated(defined),
			))
		}
	}

	validateAttributesConflict(
		cty.GetAttrPath("allowed_account_ids"),
		cty.GetAttrPath("forbidden_account_ids"),
	)(obj, cty.Path{}, &diags)

	return obj, diags
}

// Configure uses the provided configuration to set configuration fields
// within the backend.
//
// The given configuration is assumed to have already been validated
// against the schema returned by ConfigSchema and passed validation
// via PrepareConfig.
func (b *Backend) Configure(obj cty.Value) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	if obj.IsNull() {
		return diags
	}

	var region string
	if v, ok := stringAttrOk(obj, "region"); ok {
		region = v
	}

	if region != "" && !boolAttr(obj, "skip_region_validation") {
		if err := awsbase.ValidateRegion(region); err != nil {
			diags = diags.Append(tfdiags.AttributeValue(
				tfdiags.Error,
				"Invalid region value",
				err.Error(),
				cty.Path{cty.GetAttrStep{Name: "region"}},
			))
			return diags
		}
	}

	b.bucketName = stringAttr(obj, "bucket")
	b.keyName = stringAttr(obj, "key")
	b.acl = stringAttr(obj, "acl")
	b.workspaceKeyPrefix = stringAttrDefault(obj, "workspace_key_prefix", "env:")
	b.serverSideEncryption = boolAttr(obj, "encrypt")
	b.kmsKeyID = stringAttr(obj, "kms_key_id")
	b.ddbTable = stringAttr(obj, "dynamodb_table")

	if customerKey, ok := stringAttrOk(obj, "sse_customer_key"); ok {
		if len(customerKey) != 44 {
			diags = diags.Append(tfdiags.AttributeValue(
				tfdiags.Error,
				"Invalid sse_customer_key value",
				"sse_customer_key must be 44 characters in length",
				cty.Path{cty.GetAttrStep{Name: "sse_customer_key"}},
			))
		} else {
			var err error
			if b.customerEncryptionKey, err = base64.StdEncoding.DecodeString(customerKey); err != nil {
				diags = diags.Append(tfdiags.AttributeValue(
					tfdiags.Error,
					"Invalid sse_customer_key value",
					fmt.Sprintf("sse_customer_key must be base64 encoded: %s", err),
					cty.Path{cty.GetAttrStep{Name: "sse_customer_key"}},
				))
			}
		}
	} else if customerKey := os.Getenv("AWS_SSE_CUSTOMER_KEY"); customerKey != "" {
		if len(customerKey) != 44 {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid AWS_SSE_CUSTOMER_KEY value",
				`The environment variable "AWS_SSE_CUSTOMER_KEY" must be 44 characters in length`,
			))
		} else {
			var err error
			if b.customerEncryptionKey, err = base64.StdEncoding.DecodeString(customerKey); err != nil {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Invalid AWS_SSE_CUSTOMER_KEY value",
					fmt.Sprintf(`The environment variable "AWS_SSE_CUSTOMER_KEY" must be base64 encoded: %s`, err),
				))
			}
		}
	}

	cfg := &awsbase.Config{
		AccessKey:              stringAttr(obj, "access_key"),
		CallerDocumentationURL: "https://opentofu.org/docs/language/settings/backends/s3",
		CallerName:             "S3 Backend",
		SuppressDebugLog:       logging.IsDebugOrHigher(),
		IamEndpoint:            stringAttrDefaultEnvVar(obj, "iam_endpoint", "AWS_IAM_ENDPOINT"),
		MaxRetries:             intAttrDefault(obj, "max_retries", 5),
		Profile:                stringAttr(obj, "profile"),
		Region:                 stringAttr(obj, "region"),
		SecretKey:              stringAttr(obj, "secret_key"),
		SkipCredsValidation:    boolAttr(obj, "skip_credentials_validation"),
		StsEndpoint:            stringAttrDefaultEnvVar(obj, "sts_endpoint", "AWS_STS_ENDPOINT"),
		Token:                  stringAttr(obj, "token"),
		UserAgent: awsbase.UserAgentProducts{
			{Name: "APN", Version: "1.0"},
			{Name: httpclient.DefaultApplicationName, Version: version.String()},
		},
	}

	if val, ok := boolAttrOk(obj, "use_legacy_workflow"); ok {
		cfg.UseLegacyWorkflow = val
	} else {
		cfg.UseLegacyWorkflow = true
	}

	if val, ok := boolAttrOk(obj, "skip_metadata_api_check"); ok {
		if val {
			cfg.EC2MetadataServiceEnableState = imds.ClientDisabled
		} else {
			cfg.EC2MetadataServiceEnableState = imds.ClientEnabled
		}
	}

	if val, ok := stringAttrOk(obj, "shared_credentials_file"); ok {
		cfg.SharedCredentialsFiles = []string{val}
	}

	if val, ok := boolAttrOk(obj, "skip_metadata_api_check"); ok {
		if val {
			cfg.EC2MetadataServiceEnableState = imds.ClientDisabled
		} else {
			cfg.EC2MetadataServiceEnableState = imds.ClientEnabled
		}
	}

	if value := obj.GetAttr("assume_role"); !value.IsNull() {
		cfg.AssumeRole = configureNestedAssumeRole(obj)
	} else if value := obj.GetAttr("role_arn"); !value.IsNull() {
		cfg.AssumeRole = configureAssumeRole(obj)
	}

	if val, ok := stringSliceAttrDefaultEnvVarOk(obj, "shared_credentials_files", "AWS_SHARED_CREDENTIALS_FILE"); ok {
		cfg.SharedCredentialsFiles = val
	}
	if val, ok := stringSliceAttrDefaultEnvVarOk(obj, "shared_config_files", "AWS_SHARED_CONFIG_FILE"); ok {
		cfg.SharedConfigFiles = val
	}

	if val, ok := stringSliceAttrOk(obj, "allowed_account_ids"); ok {
		cfg.AllowedAccountIds = val
	}

	if val, ok := stringSliceAttrOk(obj, "forbidden_account_ids"); ok {
		cfg.ForbiddenAccountIds = val
	}

	ctx := context.TODO()
	_, awsConfig, awsDiags := awsbase.GetAwsConfig(ctx, cfg)

	for _, d := range awsDiags {
		diags = diags.Append(tfdiags.Sourceless(
			baseSeverityToTofuSeverity(d.Severity()),
			d.Summary(),
			d.Detail(),
		))
	}

	if d := verifyAllowedAccountID(ctx, awsConfig, cfg); len(d) != 0 {
		diags = diags.Append(d)
	}

	if diags.HasErrors() {
		return diags
	}

	b.awsConfig = awsConfig

	b.dynClient = dynamodb.NewFromConfig(awsConfig, getDynamoDBConfig(obj))

	b.s3Client = s3.NewFromConfig(awsConfig, getS3Config(obj))

	return diags
}

func verifyAllowedAccountID(ctx context.Context, awsConfig aws.Config, cfg *awsbase.Config) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	accountID, _, awsDiags := awsbase.GetAwsAccountIDAndPartition(ctx, awsConfig, cfg)
	for _, d := range awsDiags {
		diags = diags.Append(tfdiags.Sourceless(
			baseSeverityToTofuSeverity(d.Severity()),
			fmt.Sprintf("Retrieving AWS account details: %s", d.Summary()),
			d.Detail(),
		))
	}

	err := cfg.VerifyAccountIDAllowed(accountID)
	if err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid account ID",
			err.Error(),
		))
	}
	return diags
}

func getDynamoDBConfig(obj cty.Value) func(options *dynamodb.Options) {
	return func(options *dynamodb.Options) {
		if v, ok := stringAttrDefaultEnvVarOk(obj, "dynamodb_endpoint", "AWS_DYNAMODB_ENDPOINT", "AWS_ENDPOINT_URL_DYNAMODB"); ok {
			options.BaseEndpoint = aws.String(v)
		}
	}
}

func getS3Config(obj cty.Value) func(options *s3.Options) {
	return func(options *s3.Options) {
		if v, ok := stringAttrDefaultEnvVarOk(obj, "endpoint", "AWS_S3_ENDPOINT", "AWS_ENDPOINT_URL_S3"); ok {
			options.BaseEndpoint = aws.String(v)
		}
		if v, ok := boolAttrOk(obj, "force_path_style"); ok {
			options.UsePathStyle = v
		}
	}
}

func configureNestedAssumeRole(obj cty.Value) *awsbase.AssumeRole {
	assumeRole := awsbase.AssumeRole{}

	obj = obj.GetAttr("assume_role")
	if val, ok := stringAttrOk(obj, "role_arn"); ok {
		assumeRole.RoleARN = val
	}
	if val, ok := stringAttrOk(obj, "duration"); ok {
		dur, err := time.ParseDuration(val)
		if err != nil {
			// This should never happen because the schema should have
			// already validated the duration.
			panic(fmt.Sprintf("invalid duration %q: %s", val, err))
		}

		assumeRole.Duration = dur
	}
	if val, ok := stringAttrOk(obj, "external_id"); ok {
		assumeRole.ExternalID = val
	}

	if val, ok := stringAttrOk(obj, "policy"); ok {
		assumeRole.Policy = strings.TrimSpace(val)
	}
	if val, ok := stringSliceAttrOk(obj, "policy_arns"); ok {
		assumeRole.PolicyARNs = val
	}
	if val, ok := stringAttrOk(obj, "session_name"); ok {
		assumeRole.SessionName = val
	}
	if val, ok := stringMapAttrOk(obj, "tags"); ok {
		assumeRole.Tags = val
	}
	if val, ok := stringSliceAttrOk(obj, "transitive_tag_keys"); ok {
		assumeRole.TransitiveTagKeys = val
	}

	return &assumeRole
}

func configureAssumeRole(obj cty.Value) *awsbase.AssumeRole {
	assumeRole := awsbase.AssumeRole{}

	assumeRole.RoleARN = stringAttr(obj, "role_arn")
	assumeRole.Duration = time.Duration(intAttr(obj, "assume_role_duration_seconds") * int(time.Second))
	assumeRole.ExternalID = stringAttr(obj, "external_id")
	assumeRole.Policy = stringAttr(obj, "assume_role_policy")
	assumeRole.SessionName = stringAttr(obj, "session_name")

	if val, ok := stringSliceAttrOk(obj, "assume_role_policy_arns"); ok {
		assumeRole.PolicyARNs = val
	}
	if val, ok := stringMapAttrOk(obj, "assume_role_tags"); ok {
		assumeRole.Tags = val
	}
	if val, ok := stringSliceAttrOk(obj, "assume_role_transitive_tag_keys"); ok {
		assumeRole.TransitiveTagKeys = val
	}

	return &assumeRole
}

func stringValue(val cty.Value) string {
	v, _ := stringValueOk(val)
	return v
}

func stringValueOk(val cty.Value) (string, bool) {
	if val.IsNull() {
		return "", false
	} else {
		return val.AsString(), true
	}
}

func stringAttr(obj cty.Value, name string) string {
	return stringValue(obj.GetAttr(name))
}

func stringAttrOk(obj cty.Value, name string) (string, bool) {
	return stringValueOk(obj.GetAttr(name))
}

func stringAttrDefault(obj cty.Value, name, def string) string {
	if v, ok := stringAttrOk(obj, name); !ok {
		return def
	} else {
		return v
	}
}

func stringSliceValueOk(val cty.Value) ([]string, bool) {
	if val.IsNull() {
		return nil, false
	}

	var v []string
	if err := gocty.FromCtyValue(val, &v); err != nil {
		return nil, false
	}
	return v, true
}

func stringSliceAttrOk(obj cty.Value, name string) ([]string, bool) {
	return stringSliceValueOk(obj.GetAttr(name))
}

func stringSliceAttrDefaultEnvVarOk(obj cty.Value, name string, envvars ...string) ([]string, bool) {
	if v, ok := stringSliceAttrOk(obj, name); !ok {
		for _, envvar := range envvars {
			if ev := os.Getenv(envvar); ev != "" {
				return []string{ev}, true
			}
		}
		return nil, false
	} else {
		return v, true
	}
}

func stringAttrDefaultEnvVar(obj cty.Value, name string, envvars ...string) string {
	if v, ok := stringAttrDefaultEnvVarOk(obj, name, envvars...); !ok {
		return ""
	} else {
		return v
	}
}

func stringAttrDefaultEnvVarOk(obj cty.Value, name string, envvars ...string) (string, bool) {
	if v, ok := stringAttrOk(obj, name); !ok {
		for _, envvar := range envvars {
			if v := os.Getenv(envvar); v != "" {
				return v, true
			}
		}
		return "", false
	} else {
		return v, true
	}
}

func boolAttr(obj cty.Value, name string) bool {
	v, _ := boolAttrOk(obj, name)
	return v
}

func boolAttrOk(obj cty.Value, name string) (bool, bool) {
	if val := obj.GetAttr(name); val.IsNull() {
		return false, false
	} else {
		return val.True(), true
	}
}

func intAttr(obj cty.Value, name string) int {
	v, _ := intAttrOk(obj, name)
	return v
}

func intAttrOk(obj cty.Value, name string) (int, bool) {
	if val := obj.GetAttr(name); val.IsNull() {
		return 0, false
	} else {
		var v int
		if err := gocty.FromCtyValue(val, &v); err != nil {
			return 0, false
		}
		return v, true
	}
}

func intAttrDefault(obj cty.Value, name string, def int) int {
	if v, ok := intAttrOk(obj, name); !ok {
		return def
	} else {
		return v
	}
}

func stringMapValueOk(val cty.Value) (map[string]string, bool) {
	var m map[string]string
	err := gocty.FromCtyValue(val, &m)
	if err != nil {
		return nil, false
	}
	return m, true
}

func stringMapAttrOk(obj cty.Value, name string) (map[string]string, bool) {
	return stringMapValueOk(obj.GetAttr(name))
}

func pathString(path cty.Path) string {
	var buf strings.Builder
	for i, step := range path {
		switch x := step.(type) {
		case cty.GetAttrStep:
			if i != 0 {
				buf.WriteString(".")
			}
			buf.WriteString(x.Name)
		case cty.IndexStep:
			val := x.Key
			typ := val.Type()
			var s string
			switch {
			case typ == cty.String:
				s = val.AsString()
			case typ == cty.Number:
				num := val.AsBigFloat()
				if num.IsInt() {
					s = num.Text('f', -1)
				} else {
					s = num.String()
				}
			default:
				s = fmt.Sprintf("<unexpected index: %s>", typ.FriendlyName())
			}
			buf.WriteString(fmt.Sprintf("[%s]", s))
		default:
			if i != 0 {
				buf.WriteString(".")
			}
			buf.WriteString(fmt.Sprintf("<unexpected step: %[1]T %[1]v>", x))
		}
	}
	return buf.String()
}

func findDeprecatedFields(obj cty.Value, attrs map[string]string) map[string]string {
	defined := make(map[string]string)
	for attr, v := range attrs {
		if val := obj.GetAttr(attr); !val.IsNull() {
			defined[attr] = v
		}
	}
	return defined
}

func formatDeprecated(attrs map[string]string) string {
	var maxLen int
	var buf strings.Builder

	names := make([]string, 0, len(attrs))
	for deprecated, replacement := range attrs {
		names = append(names, deprecated)
		if l := len(deprecated); l > maxLen {
			maxLen = l
		}

		fmt.Fprintf(&buf, "  * %-[1]*[2]s -> %s\n", maxLen, deprecated, replacement)
	}

	sort.Strings(names)

	return buf.String()
}

const encryptionKeyConflictError = `Only one of "kms_key_id" and "sse_customer_key" can be set.

The "kms_key_id" is used for encryption with KMS-Managed Keys (SSE-KMS)
while "sse_customer_key" is used for encryption with customer-managed keys (SSE-C).
Please choose one or the other.`

const encryptionKeyConflictEnvVarError = `Only one of "kms_key_id" and the environment variable "AWS_SSE_CUSTOMER_KEY" can be set.

The "kms_key_id" is used for encryption with KMS-Managed Keys (SSE-KMS)
while "AWS_SSE_CUSTOMER_KEY" is used for encryption with customer-managed keys (SSE-C).
Please choose one or the other.`
