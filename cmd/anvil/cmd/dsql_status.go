package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

// A DSQL component's schema, roles and grants are applied after `up`, by its
// bootstrap Lambda reading the roles table's stream. The Lambda writes a
// status item next to each item it applies (sk "STATUS#<item sk>"), carrying
// the item's version. After `up`, the CLI waits until every item's status
// matches its current version, prints the results, and fails the deploy when
// one failed. Unchanged items keep their earlier status, so a failure stays
// visible on every deploy until the code or database is fixed.

const (
	dbStatusPrefix       = "STATUS#"
	dbSettingsKey        = "anvil#settings" // must match the provider and Lambda
	dbStatusPollInterval = 3 * time.Second
	dbStatusWaitTimeout  = 3 * time.Minute
)

type dbStatus struct {
	State     string
	Message   string
	Version   string
	UpdatedAt time.Time
}

// dbItemResult is one roles-table item and its latest status.
type dbItemResult struct {
	PK, SK  string
	Version string
	Status  *dbStatus // nil when the Lambda hasn't reported yet
}

func (r dbItemResult) confirmed() bool {
	return r.Status != nil && r.Status.Version == r.Version
}

// label names an item the way the user declared it.
func (r dbItemResult) label() string {
	switch {
	case r.PK == dbSettingsKey:
		return "roles"
	case strings.HasPrefix(r.PK, "table#"):
		return "table " + strings.TrimPrefix(r.PK, "table#")
	case strings.HasPrefix(r.SK, "arn:"):
		return fmt.Sprintf("access %s → %s", r.SK[strings.LastIndex(r.SK, "/")+1:], r.PK)
	default:
		return "role " + r.PK
	}
}

func attrS(item map[string]types.AttributeValue, name string) string {
	if v, ok := item[name].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

// dbItemResults pairs each versioned item in a roles table with its status.
// An item with no version (written by an older provider, or not by Anvil at
// all) isn't waited on, but is still reported when its status is failed or
// drift, so an item written straight to the table can't fail silently.
func dbItemResults(items []map[string]types.AttributeValue) []dbItemResult {
	statuses := map[string]*dbStatus{}
	var out, unversioned []dbItemResult
	for _, item := range items {
		pk, sk := attrS(item, "roleName"), attrS(item, "sk")
		if strings.HasPrefix(sk, dbStatusPrefix) {
			st := &dbStatus{State: attrS(item, "state"), Message: attrS(item, "message"), Version: attrS(item, "version")}
			st.UpdatedAt, _ = time.Parse(time.RFC3339, attrS(item, "updatedAt"))
			statuses[pk+"\x00"+strings.TrimPrefix(sk, dbStatusPrefix)] = st
			continue
		}
		if v := attrS(item, "version"); v != "" {
			out = append(out, dbItemResult{PK: pk, SK: sk, Version: v})
		} else {
			unversioned = append(unversioned, dbItemResult{PK: pk, SK: sk})
		}
	}
	for i := range out {
		out[i].Status = statuses[out[i].PK+"\x00"+out[i].SK]
	}
	for _, r := range unversioned {
		if st := statuses[r.PK+"\x00"+r.SK]; st != nil && (st.State == "failed" || st.State == "drift") {
			r.Status, r.Version = st, st.Version
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].label() < out[j].label() })
	return out
}

func scanRolesTable(ctx context.Context, client *dynamodb.Client, table string) ([]map[string]types.AttributeValue, error) {
	var items []map[string]types.AttributeValue
	pages := dynamodb.NewScanPaginator(client, &dynamodb.ScanInput{TableName: aws.String(table), ConsistentRead: aws.Bool(true)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
	}
	return items, nil
}

// waitForDatabaseChanges runs after a successful `up`. It returns an error
// when any database change failed; changes not confirmed in time only warn.
func waitForDatabaseChanges(ctx context.Context, s auto.Stack, stage string, since time.Time) error {
	components, err := stackDSQLComponents(ctx, s, stage)
	if err != nil {
		return err
	}
	failed := 0
	for _, c := range components {
		if c.RolesTable == "" {
			continue
		}
		n, err := waitForComponent(ctx, c, resolveRegionForStage(stage), since)
		if err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		failed += n
	}
	if failed > 0 {
		return fmt.Errorf("%d database change(s) failed — the database doesn't match your code", failed)
	}
	return nil
}

func waitForComponent(ctx context.Context, c dsqlStreamComponent, region string, since time.Time) (failed int, err error) {
	cfg, err := awsConfigFor(ctx, region)
	if err != nil {
		return 0, err
	}
	client := dynamodb.NewFromConfig(cfg)

	var results []dbItemResult
	deadline := time.Now().Add(dbStatusWaitTimeout)
	announced := false
	for {
		items, err := scanRolesTable(ctx, client, c.RolesTable)
		if err != nil {
			return 0, fmt.Errorf("reading database change status: %w", err)
		}
		results = dbItemResults(items)
		pending := 0
		for _, r := range results {
			if !r.confirmed() {
				pending++
			}
		}
		if pending == 0 || time.Now().After(deadline) {
			break
		}
		if !announced {
			fmt.Printf("  Applying database changes for %s...\n", c.Name)
			announced = true
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(dbStatusPollInterval):
		}
	}
	return printDatabaseChanges(c.Name, results, since), nil
}

// printDatabaseChanges prints what this deploy changed plus every item still
// failed or drifted, and returns how many failed.
func printDatabaseChanges(component string, results []dbItemResult, since time.Time) (failed int) {
	var unconfirmed []string
	for _, r := range results {
		switch {
		case !r.confirmed():
			unconfirmed = append(unconfirmed, r.label())
		case r.Status.State == "failed":
			failed++
			printFail(fmt.Sprintf("%s: %s", r.label(), r.Status.Message))
		case r.Status.State == "drift":
			printWarn(fmt.Sprintf("%s: %s", r.label(), r.Status.Message))
		case !r.Status.UpdatedAt.Before(since):
			printCheck(fmt.Sprintf("%s: %s", r.label(), r.Status.Message))
		}
	}
	if len(unconfirmed) > 0 {
		printWarn(fmt.Sprintf("%s: database changes not confirmed after %s, check the bootstrap Lambda's logs: %s",
			component, dbStatusWaitTimeout, strings.Join(unconfirmed, ", ")))
	}
	return failed
}

func printFail(msg string) {
	if isTTY() {
		fmt.Printf("  %s %s\n", red("✘"), msg)
	} else {
		fmt.Printf("  [fail] %s\n", msg)
	}
}
