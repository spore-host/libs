package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/dynamodbtags"
)

// TableTags returns the tags the lifecycle table is created with in region:
// the standard set with ManagedBy=ToolShared, plus Role=service and
// Component=lifecycle. ToolShared, not any one tool, because every tool
// writes to the table; Role=service so that no footprint teardown treats it
// as something to delete. Only CloseTableIfEmpty removes it.
func (c *Client) TableTags(region string, now time.Time) map[string]string {
	t := tags.For(tags.ToolShared, region, now, c.o.Owner)
	t[tags.Role] = tags.RoleService
	t[tags.Component] = "lifecycle"
	return t
}

func (c *Client) createTableInput(region string) *dynamodb.CreateTableInput {
	return &dynamodb.CreateTableInput{
		TableName:            aws.String(TableName),
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String(attrPK), AttributeType: types.ScalarAttributeTypeS}},
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String(attrPK), KeyType: types.KeyTypeHash}},
		BillingMode:          types.BillingModePayPerRequest,
		Tags:                 dynamodbtags.FromMap(c.TableTags(region, c.now())),
	}
}

// ensureTable returns once the table in region accepts writes. A table that
// is DELETING is waited out and then created; one that is CREATING is waited
// for. Of several callers creating it at once, one wins and the others see
// ResourceInUseException, which is fine. It gives up with a *WaitError once
// start is Options.WaitTimeout ago.
func (c *Client) ensureTable(ctx context.Context, a API, region string, tool tags.Tool, scope string, start time.Time) error {
	for polls := 0; ; polls++ {
		out, err := a.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(TableName)})
		status := types.TableStatus("")
		switch {
		case isNotFound(err):
			created, err := a.CreateTable(ctx, c.createTableInput(region))
			if err != nil && !isInUse(err) {
				return fmt.Errorf("lifecycle: create %s in %s: %w", TableName, region, err)
			}
			status = types.TableStatusCreating
			if err == nil && created != nil && created.TableDescription != nil && created.TableDescription.TableStatus != "" {
				status = created.TableDescription.TableStatus
			}
		case err != nil:
			return fmt.Errorf("lifecycle: describe %s in %s: %w", TableName, region, err)
		case out != nil && out.Table != nil:
			status = out.Table.TableStatus
		}
		switch status {
		case types.TableStatusActive, types.TableStatusUpdating:
			return nil
		case types.TableStatusCreating, types.TableStatusDeleting:
		default:
			return fmt.Errorf("lifecycle: %s in %s is %q", TableName, region, status)
		}
		if waited := max(c.now().Sub(start), time.Duration(polls)*c.o.TablePollInterval); waited >= c.o.WaitTimeout {
			return &WaitError{Tool: tool, Scope: scope, Waited: waited, Blocker: "table " + string(status)}
		}
		if err := c.o.Sleep(ctx, c.o.TablePollInterval); err != nil {
			return err
		}
	}
}

// CloseTableIfEmpty deletes the lifecycle table in region if no tool item is
// left in it. It reports whether the table is gone (deleted now, or already
// absent).
//
// It first writes the closing sentinel, on condition that there is none (or
// only a stale one), so no creator can add an item from then on; then scans
// with strong consistency. If any item other than the sentinel remains, it
// removes the sentinel and reports false. Otherwise it deletes the table. If
// another caller holds a fresh sentinel it reports false and does nothing.
func (c *Client) CloseTableIfEmpty(ctx context.Context, region string) (bool, error) {
	a, err := c.api(region)
	if err != nil {
		return false, err
	}
	token, err := c.newToken()
	if err != nil {
		return false, err
	}
	now := c.now()
	_, err = a.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(TableName),
		Item: map[string]types.AttributeValue{
			attrPK:    s(SentinelKey),
			attrToken: s(token),
			attrOwner: s(c.o.Owner),
			attrSince: s(formatTime(now)),
		},
		ConditionExpression:       aws.String("attribute_not_exists(#pk) OR #since < :stale"),
		ExpressionAttributeNames:  names(attrPK, attrSince),
		ExpressionAttributeValues: map[string]types.AttributeValue{":stale": s(c.staleBefore(now))},
	})
	switch {
	case isNotFound(err):
		// Absent, or mid-transition: only absent counts as gone.
		_, derr := a.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(TableName)})
		if isNotFound(derr) {
			return true, nil
		}
		return false, nil
	case isCCF(err):
		return false, nil // another caller is closing it
	case err != nil:
		return false, fmt.Errorf("lifecycle: write closing sentinel in %s: %w", region, err)
	}

	remaining, err := c.countItems(ctx, a)
	if err != nil {
		return false, c.dropSentinel(ctx, a, token, fmt.Errorf("lifecycle: scan %s in %s: %w", TableName, region, err))
	}
	if remaining > 0 {
		return false, c.dropSentinel(ctx, a, token, nil)
	}
	if _, err := a.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(TableName)}); err != nil {
		if isNotFound(err) {
			return true, nil
		}
		return false, c.dropSentinel(ctx, a, token, fmt.Errorf("lifecycle: delete %s in %s: %w", TableName, region, err))
	}
	return true, nil
}

// countItems counts the items other than the sentinel, with a strongly
// consistent scan.
func (c *Client) countItems(ctx context.Context, a API) (int, error) {
	var start map[string]types.AttributeValue
	count := 0
	for {
		out, err := a.Scan(ctx, &dynamodb.ScanInput{
			TableName:                aws.String(TableName),
			ConsistentRead:           aws.Bool(true),
			ProjectionExpression:     aws.String("#pk"),
			ExpressionAttributeNames: names(attrPK),
			ExclusiveStartKey:        start,
		})
		if err != nil {
			return 0, err
		}
		for _, it := range out.Items {
			if str(it, attrPK) != SentinelKey {
				count++
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			return count, nil
		}
		start = out.LastEvaluatedKey
	}
}

// dropSentinel removes this caller's sentinel, so creators are not blocked
// until it goes stale. It runs even if ctx is cancelled.
func (c *Client) dropSentinel(ctx context.Context, a API, token string, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, err := a.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:                 aws.String(TableName),
		Key:                       keyOf(SentinelKey),
		ConditionExpression:       aws.String("#token = :token"),
		ExpressionAttributeNames:  names(attrToken),
		ExpressionAttributeValues: map[string]types.AttributeValue{":token": s(token)},
	})
	if err != nil && !isCCF(err) {
		err = fmt.Errorf("lifecycle: remove closing sentinel: %w", err)
		if cause != nil {
			return fmt.Errorf("%w (and %w)", cause, err)
		}
		return err
	}
	return cause
}
