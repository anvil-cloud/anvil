package apigateway

import (
	"fmt"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/acm"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/apigatewayv2"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudwatch"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/route53"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// DomainConfig holds the resolved domain configuration for an API Gateway component.
// Shared across HTTP, REST, and WebSocket API types.
type DomainConfig struct {
	// Name is the fully qualified domain name, e.g. "api.mysite.com"
	Name string

	// BasePath is the optional base path mapping, e.g. "v1".
	// When set, the API is accessible at domain/basePath.
	// Omit for root path mapping.
	BasePath string

	// CertificateArn is the ACM certificate ARN for the domain.
	// When empty, Anvil creates and validates the certificate automatically.
	CertificateArn string

	// ZoneId is the Route 53 hosted zone ID for DNS record creation.
	// When empty, Anvil looks up the zone by domain name automatically.
	ZoneId string

	// Dns controls whether Anvil creates Route 53 DNS and cert validation records.
	// When nil or true, Route 53 records are created automatically.
	// When explicitly false, all Route 53 calls are skipped — use for Cloudflare
	// or other non-Route 53 DNS providers. CertValidationCname will be populated
	// when dns: false and no certificateArn is provided.
	Dns *bool
}

// LogConfig holds the resolved CloudWatch log configuration for an API Gateway component.
type LogConfig struct {
	// RetentionDays is the number of days to retain access logs.
	// Defaults to 365 (1 year).
	RetentionDays int
}

// BaseOutputs holds the shared outputs produced by SetupBase.
// Consumed by HTTP, REST, and WebSocket API constructors.
type BaseOutputs struct {
	// LogGroupArn is the ARN of the CloudWatch log group for access logs.
	LogGroupArn pulumi.StringOutput

	// CertificateArn is the ARN of the ACM certificate for the custom domain.
	// Empty string output when no domain is configured.
	CertificateArn pulumi.StringOutput

	// DomainNameId is the API Gateway domain name resource ID.
	// Empty string output when no domain is configured.
	DomainNameId pulumi.StringOutput

	// ApiGatewayDomainTarget is the API Gateway regional domain name for DNS CNAME.
	// Empty string output when no domain is configured.
	ApiGatewayDomainTarget pulumi.StringOutput

	// HostedZoneId is the Route 53 hosted zone ID used for the DNS record.
	// Empty string output when no domain is configured.
	HostedZoneId pulumi.StringOutput

	// CertValidationCname is the ACM DNS validation CNAME record details.
	// Only populated when dns: false and certificateArn is omitted — Anvil creates
	// the cert but cannot add the validation record automatically.
	// Add this CNAME in Cloudflare (or your DNS provider) then re-run deploy.
	// Nil when dns: true (Route 53 handles validation automatically) or when
	// a BYO certificateArn is provided.
	CertValidationCname pulumi.AnyOutput
}

// SetupBase provisions the shared infrastructure used by all API Gateway types:
//   - CloudWatch log group with smart lifecycle retention
//   - ACM certificate (when domain is configured)
//   - API Gateway custom domain name resource (when domain is configured)
//   - Route 53 DNS A alias record (when domain is configured)
//
// The log group and domain resources are always created as children of parent.
// Callers wire the log group ARN and domain name ID into their specific API resource.
func SetupBase(
	ctx *pulumi.Context,
	name string,
	stage string,
	stageId string,
	domain *DomainConfig,
	logRetentionDays int,
	parent pulumi.Resource,
) (*BaseOutputs, error) {
	out := &BaseOutputs{}

	// ── 1. CloudWatch Log Group ────────────────────────────────────────────────
	//
	// Access logs capture per-request data: method, path, status, latency,
	// source IP, and request ID. On by default — required by SOC 2, ISO 27001,
	// and PCI DSS for audit trail evidence.
	//
	// Retention defaults to 365 days (1 year) — satisfies the baseline retention
	// requirement for SOC 2, ISO 27001, and PCI DSS. Override via logRetention.
	logGroupName := fmt.Sprintf("/aws/apigateway/%s-%s-%s", stage, name, stageId)

	if logRetentionDays == 0 {
		logRetentionDays = 365
	}

	logGroup, err := cloudwatch.NewLogGroup(ctx, name+"-logs", &cloudwatch.LogGroupArgs{
		Name:            pulumi.String(logGroupName),
		RetentionInDays: pulumi.Int(logRetentionDays),
		Tags: pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return nil, fmt.Errorf("failed to create API Gateway log group: %w", err)
	}

	out.LogGroupArn = logGroup.Arn

	// ── 2. Custom Domain (optional) ───────────────────────────────────────────
	//
	// When a domain is configured Anvil provisions:
	//   a. ACM certificate with DNS validation (us-east-1 for edge, regional otherwise)
	//   b. API Gateway domain name resource (regional endpoint)
	//   c. Route 53 A alias record pointing to the API Gateway regional domain
	//
	// When no domain is set all domain outputs are empty strings and the
	// execute-api endpoint remains enabled for direct access.
	if domain != nil && domain.Name != "" {
		skipDns := domain.Dns != nil && !*domain.Dns

		certArn, validationCname, err := setupCertificate(ctx, name, domain, skipDns, parent)
		if err != nil {
			return nil, err
		}
		out.CertificateArn = certArn
		out.CertValidationCname = validationCname

		domainNameId, apiGwTarget, hostedZoneId, err := setupDomainName(ctx, name, domain, certArn, parent)
		if err != nil {
			return nil, err
		}
		out.DomainNameId = domainNameId
		out.ApiGatewayDomainTarget = apiGwTarget
		out.HostedZoneId = hostedZoneId

		if !skipDns {
			if err := setupDnsRecord(ctx, name, domain, apiGwTarget, hostedZoneId, parent); err != nil {
				return nil, err
			}
		}
	} else {
		// No domain — emit empty outputs so callers don't need nil checks.
		out.CertificateArn = pulumi.String("").ToStringOutput()
		out.DomainNameId = pulumi.String("").ToStringOutput()
		out.ApiGatewayDomainTarget = pulumi.String("").ToStringOutput()
		out.HostedZoneId = pulumi.String("").ToStringOutput()
		out.CertValidationCname = pulumi.Any(nil)
	}

	return out, nil
}

type CertValidationCname struct {
	Name  string
	Value string
}

// setupCertificate creates or reuses an ACM certificate for the given domain.
// If domain.CertificateArn is set, it is used directly (BYO cert).
// Otherwise Anvil creates a new certificate with DNS validation.
func setupCertificate(
	ctx *pulumi.Context,
	name string,
	domain *DomainConfig,
	skipDns bool,
	parent pulumi.Resource,
) (pulumi.StringOutput, pulumi.AnyOutput, error) {
	// BYO cert — use the provided ARN directly, no validation needed.
	if domain.CertificateArn != "" {
		return pulumi.String(domain.CertificateArn).ToStringOutput(), pulumi.Any(nil), nil
	}

	cert, err := acm.NewCertificate(ctx, name+"-cert", &acm.CertificateArgs{
		DomainName:       pulumi.String(domain.Name),
		ValidationMethod: pulumi.String("DNS"),
		Tags: pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return pulumi.StringOutput{}, pulumi.AnyOutput{}, fmt.Errorf("failed to create ACM certificate for %s: %w", domain.Name, err)
	}

	validationOption := cert.DomainValidationOptions.Index(pulumi.Int(0))

	validationCname := pulumi.All(
		validationOption.ResourceRecordName(),
		validationOption.ResourceRecordValue(),
	).ApplyT(func(args []interface{}) interface{} {
		name, _ := args[0].(*string)
		value, _ := args[1].(*string)
		if name == nil || value == nil {
			return nil
		}
		return CertValidationCname{Name: *name, Value: *value}
	}).(pulumi.AnyOutput)

	if skipDns {
		// dns: false — user must add the validation CNAME in Cloudflare manually.
		// Block the deploy on CertificateValidation with no ValidationRecordFqdns —
		// ACM will validate once the user adds the record.
		validation, err := acm.NewCertificateValidation(ctx, name+"-cert-validated", &acm.CertificateValidationArgs{
			CertificateArn: cert.Arn,
		}, pulumi.Parent(parent))
		if err != nil {
			return pulumi.StringOutput{}, pulumi.AnyOutput{}, fmt.Errorf("failed to create certificate validation: %w", err)
		}
		return validation.CertificateArn, validationCname, nil
	}

	// Route 53 flow — create validation record automatically.
	validationRecord, err := route53.NewRecord(ctx, name+"-cert-validation", &route53.RecordArgs{
		ZoneId: lookupZoneId(ctx, name, domain),
		Name: validationOption.ResourceRecordName().ApplyT(func(v interface{}) string {
			if v == nil {
				return ""
			}
			return *v.(*string)
		}).(pulumi.StringOutput),
		Type: validationOption.ResourceRecordType().ApplyT(func(v interface{}) string {
			if v == nil {
				return "CNAME"
			}
			return *v.(*string)
		}).(pulumi.StringOutput),
		Records: pulumi.StringArray{
			validationOption.ResourceRecordValue().ApplyT(func(v interface{}) string {
				if v == nil {
					return ""
				}
				return *v.(*string)
			}).(pulumi.StringOutput),
		},
		Ttl: pulumi.Int(60),
	}, pulumi.Parent(parent))
	if err != nil {
		return pulumi.StringOutput{}, pulumi.AnyOutput{}, fmt.Errorf("failed to create certificate validation record: %w", err)
	}

	validation, err := acm.NewCertificateValidation(ctx, name+"-cert-validated", &acm.CertificateValidationArgs{
		CertificateArn: cert.Arn,
		ValidationRecordFqdns: pulumi.StringArray{
			validationRecord.Fqdn,
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return pulumi.StringOutput{}, pulumi.AnyOutput{}, fmt.Errorf("failed to validate certificate: %w", err)
	}

	return validation.CertificateArn, pulumi.Any(nil), nil
}

// setupDomainName creates the API Gateway v2 domain name resource.
// Returns the resource ID, the regional API Gateway target domain, and the hosted zone ID.
func setupDomainName(
	ctx *pulumi.Context,
	name string,
	domain *DomainConfig,
	certArn pulumi.StringOutput,
	parent pulumi.Resource,
) (pulumi.StringOutput, pulumi.StringOutput, pulumi.StringOutput, error) {
	dn, err := apigatewayv2.NewDomainName(ctx, name+"-domain", &apigatewayv2.DomainNameArgs{
		DomainName: pulumi.String(domain.Name),
		DomainNameConfiguration: &apigatewayv2.DomainNameDomainNameConfigurationArgs{
			CertificateArn: certArn,
			EndpointType:   pulumi.String("REGIONAL"),
			// TLS 1.2 minimum — enforced always. TLS 1.0 and 1.1 are deprecated
			// (RFC 8996) and fail PCI DSS 4.0, SOC 2, and ISO 27001 audits.
			SecurityPolicy: pulumi.String("TLS_1_2"),
		},
		Tags: pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return pulumi.StringOutput{}, pulumi.StringOutput{}, pulumi.StringOutput{},
			fmt.Errorf("failed to create API Gateway domain name: %w", err)
	}

	apiGwTarget := dn.DomainNameConfiguration.ApplyT(func(cfg apigatewayv2.DomainNameDomainNameConfiguration) string {
		if cfg.TargetDomainName == nil {
			return ""
		}
		return *cfg.TargetDomainName
	}).(pulumi.StringOutput)

	apiGwHostedZone := dn.DomainNameConfiguration.ApplyT(func(cfg apigatewayv2.DomainNameDomainNameConfiguration) string {
		if cfg.HostedZoneId == nil {
			return ""
		}
		return *cfg.HostedZoneId
	}).(pulumi.StringOutput)

	return dn.ID().ToStringOutput(), apiGwTarget, apiGwHostedZone, nil
}

// setupDnsRecord creates the Route 53 A alias record pointing to API Gateway.
func setupDnsRecord(
	ctx *pulumi.Context,
	name string,
	domain *DomainConfig,
	apiGwTarget pulumi.StringOutput,
	apiGwHostedZoneId pulumi.StringOutput,
	parent pulumi.Resource,
) error {
	zoneId := lookupZoneId(ctx, name, domain)

	_, err := route53.NewRecord(ctx, name+"-dns", &route53.RecordArgs{
		ZoneId: zoneId,
		Name:   pulumi.String(domain.Name),
		Type:   pulumi.String("A"),
		Aliases: route53.RecordAliasArray{
			&route53.RecordAliasArgs{
				Name:                 apiGwTarget,
				ZoneId:               apiGwHostedZoneId,
				EvaluateTargetHealth: pulumi.Bool(false),
			},
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to create DNS record for %s: %w", domain.Name, err)
	}

	return nil
}

// lookupZoneId resolves the Route 53 hosted zone ID for the domain.
// Uses domain.ZoneId directly if provided, otherwise looks up by domain name.
func lookupZoneId(ctx *pulumi.Context, name string, domain *DomainConfig) pulumi.StringInput {
	if domain.ZoneId != "" {
		return pulumi.String(domain.ZoneId)
	}

	// Auto-discover zone by extracting the apex domain from the full domain name.
	// e.g. "api.mysite.com" -> look up zone for "mysite.com"
	zone, err := route53.LookupZone(ctx, &route53.LookupZoneArgs{
		Name: &domain.Name,
	}, nil)
	if err != nil {
		// Return the domain name as fallback — Pulumi will surface the error
		// at preview/apply time with full context.
		return pulumi.String(domain.Name)
	}

	return pulumi.String(zone.ZoneId)
}

// ResolveDays converts an Anvil log retention preset string to days.
// Mirrors the Lambda log retention presets for consistency.
func ResolveDays(preset string) int {
	switch preset {
	case "7d":
		return 7
	case "30d":
		return 30
	case "90d":
		return 90
	case "1y":
		return 365
	case "3y":
		return 1095
	case "6y":
		return 2190
	case "7y":
		return 2555
	default:
		return 365
	}
}

// SanitizeRouteName converts a route path to a safe Pulumi resource name suffix.
// e.g. "/users/{id}" -> "users-id"
func SanitizeRouteName(path string) string {
	result := make([]byte, 0, len(path))
	for i := 0; i < len(path); i++ {
		c := path[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			result = append(result, c)
		} else if len(result) > 0 && result[len(result)-1] != '-' {
			result = append(result, '-')
		}
	}
	// Trim trailing dash
	for len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}
	return string(result)
}

// ValidateMethod checks that the HTTP method is a valid API Gateway method.
func ValidateMethod(method string) error {
	valid := map[string]bool{
		"GET": true, "POST": true, "PUT": true,
		"PATCH": true, "DELETE": true, "ANY": true,
	}
	if !valid[method] {
		return fmt.Errorf("invalid HTTP method %q — must be one of GET, POST, PUT, PATCH, DELETE, ANY", method)
	}
	return nil
}

// ValidateConsumerMethod enforces that non-GET-safe consumers use POST.
// SQS, EventBridge, and Step Functions are write operations — GET on these
// consumers is a configuration error that Anvil catches at deploy time.
func ValidateConsumerMethod(method string, consumerType string) error {
	readOnlyMethods := map[string]bool{"GET": true}
	writeOnlyConsumers := map[string]bool{
		"sqs": true, "eventBridge": true, "stepFunctions": true,
	}
	if writeOnlyConsumers[consumerType] && readOnlyMethods[method] {
		return fmt.Errorf(
			"route method %q is invalid for consumer type %q — "+
				"SQS, EventBridge, and Step Functions are write operations and must use POST, PUT, or PATCH",
			method, consumerType,
		)
	}
	return nil
}

// InferAllowMethods extracts unique HTTP methods from a set of routes.
// Used to populate CORS allowMethods automatically without user declaration.
func InferAllowMethods(methods []string) []string {
	seen := make(map[string]bool)
	result := []string{}
	for _, m := range methods {
		if m == "ANY" {
			// ANY expands to all methods for CORS purposes
			return []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
		}
		if !seen[m] {
			seen[m] = true
			result = append(result, m)
		}
	}
	return result
}

// DefaultLogFormat is the structured JSON access log format written to CloudWatch.
// Captures the fields required by SOC 2, ISO 27001, and PCI DSS auditors:
//   - requestId: unique request identifier for correlation
//   - sourceIp: client IP for security analysis
//   - requestTime: timestamp for audit trail
//   - httpMethod + routeKey: what was called
//   - status: response code
//   - responseLatency: performance baseline
//   - integrationLatency: backend performance
//   - userAgent: client identification
const DefaultLogFormat = `{"requestId":"$context.requestId","sourceIp":"$context.identity.sourceIp","requestTime":"$context.requestTime","httpMethod":"$context.httpMethod","routeKey":"$context.routeKey","status":"$context.status","protocol":"$context.protocol","responseLatency":"$context.responseLatency","integrationLatency":"$context.integrationLatency","userAgent":"$context.identity.userAgent"}`

// unused import guard — provider is used by callers, referenced here to keep
// the import available for shared helpers that reference provider.PhysicalName.
var _ = provider.PhysicalName
