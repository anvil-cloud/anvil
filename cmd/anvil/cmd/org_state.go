package cmd

// The Org state bucket: one per organization, in management, created on first
// use and hardened on every run.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// orgRetainVersions is how many deploys' state the Org bucket keeps (App: 3).
const orgRetainVersions = 10

// orgStateBucketName is deterministic per organization so a lost
// anvil.org.yaml can't orphan the state. Org IDs are lowercase and unique.
func orgStateBucketName(orgID string) string {
	return sanitizeBucketPart("anvil-org-state-" + orgID)
}

// ensureOrgStateBucket returns the Org state bucket, creating and hardening it
// in the management account on first use.
func ensureOrgStateBucket(ctx context.Context, cfg aws.Config, file *orgFileConfig, org *orgInfo, region string) (string, error) {
	client := s3.NewFromConfig(cfg)

	if file.State != nil && file.State.Bucket != "" {
		bucket := file.State.Bucket
		if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
			return "", fmt.Errorf("Org state bucket %s not found in %s.\n"+
				"  If it was deleted, Org has lost track of what it manages. Restore the bucket, or remove\n"+
				"  state: from %s to start again (existing OUs will then collide in preview).", bucket, region, orgConfigFile)
		}
		if err := hardenOrgStateBucket(ctx, client, bucket, file.Project, org); err != nil {
			return "", err
		}
		return bucket, nil
	}

	bucket := orgStateBucketName(org.ID)
	input := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if region != "us-east-1" {
		input.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}
	if _, err := client.CreateBucket(ctx, input); err != nil {
		var owned *s3types.BucketAlreadyOwnedByYou
		var taken *s3types.BucketAlreadyExists
		switch {
		case errors.As(err, &owned):
			// Created by an earlier run whose anvil.org.yaml wasn't kept.
		case errors.As(err, &taken):
			return "", fmt.Errorf("S3 bucket name %s is taken by another AWS account.\n"+
				"  Add state: { bucket: <a name you own> } to %s and try again.", bucket, orgConfigFile)
		default:
			return "", fmt.Errorf("failed to create Org state bucket: %s", mapError(err.Error()))
		}
	} else {
		printCheck(fmt.Sprintf("Created Org state bucket %s", bucket))
	}

	if err := hardenOrgStateBucket(ctx, client, bucket, file.Project, org); err != nil {
		return "", err
	}
	return bucket, nil
}

// hardenOrgStateBucket applies everything an App state bucket gets, plus the
// Org-only protections that are on by default: deletes of old versions and of
// the bucket itself are denied. Idempotent; reapplied on every run.
func hardenOrgStateBucket(ctx context.Context, client *s3.Client, bucket, project string, org *orgInfo) error {
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	}); err != nil {
		return fmt.Errorf("failed to enable versioning on the Org state bucket: %w", err)
	}
	if err := putStateLifecycle(ctx, client, bucket, stateSettings{RetainVersions: orgRetainVersions, CompressState: true}); err != nil {
		return fmt.Errorf("failed to set Org state retention: %w", err)
	}
	if _, err := client.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
		Bucket: aws.String(bucket),
		ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
			Rules: []s3types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
					SSEAlgorithm: s3types.ServerSideEncryptionAes256,
				},
			}},
		},
	}); err != nil {
		return fmt.Errorf("failed to enable encryption on the Org state bucket: %w", err)
	}
	if _, err := client.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: aws.String(bucket),
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(true),
			BlockPublicPolicy:     aws.Bool(true),
			IgnorePublicAcls:      aws.Bool(true),
			RestrictPublicBuckets: aws.Bool(true),
		},
	}); err != nil {
		return fmt.Errorf("failed to block public access on the Org state bucket: %w", err)
	}
	if _, err := client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucket),
		Policy: aws.String(orgStateBucketPolicy(bucket)),
	}); err != nil {
		return fmt.Errorf("failed to apply the Org state bucket policy: %w", err)
	}
	if _, err := ensureBucketTags(ctx, client, bucket, map[string]string{
		"ManagedBy":     "anvil",
		"Component":     "OrgStateBucket",
		"anvil:managed": "true",
		"anvil:org":     project,
	}); err != nil {
		return fmt.Errorf("failed to tag the Org state bucket: %w", err)
	}
	return nil
}

// orgStateBucketPolicy is TLS-only plus hardening (1): no one deletes old
// state versions or the bucket. Lifecycle rules aren't subject to bucket
// policies, so retention still works.
func orgStateBucketPolicy(bucket string) string {
	arn := "arn:aws:s3:::" + bucket
	policy := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Sid":       "DenyNonTLS",
				"Effect":    "Deny",
				"Principal": "*",
				"Action":    "s3:*",
				"Resource":  []string{arn, arn + "/*"},
				"Condition": map[string]any{"Bool": map[string]string{"aws:SecureTransport": "false"}},
			},
			{
				"Sid":       "DenyDeletingStateHistory",
				"Effect":    "Deny",
				"Principal": "*",
				"Action":    []string{"s3:DeleteObjectVersion", "s3:DeleteBucket"},
				"Resource":  []string{arn, arn + "/*"},
			},
		},
	}
	data, _ := json.Marshal(policy)
	return string(data)
}
