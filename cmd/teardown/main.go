// Command teardown terminates every MicroVM in a region, deletes the named images,
// and waits until no environment is left running. It uses the ambient AWS credentials.
//
// Usage: go run ./cmd/teardown <region> [image-arn...]
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
)

type microVMAPI interface {
	ListMicrovms(context.Context, *lambdamicrovms.ListMicrovmsInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ListMicrovmsOutput, error)
	TerminateMicrovm(context.Context, *lambdamicrovms.TerminateMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error)
	GetMicrovm(context.Context, *lambdamicrovms.GetMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error)
	DeleteMicrovmImage(context.Context, *lambdamicrovms.DeleteMicrovmImageInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.DeleteMicrovmImageOutput, error)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: teardown <region> [image-arn...]")
		os.Exit(2)
	}
	ctx := context.Background()
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(os.Args[1]))
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}
	if err := run(ctx, lambdamicrovms.NewFromConfig(awsConfig), os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "teardown:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, client microVMAPI, images []string) error {
	live, terminated, err := terminateAll(ctx, client)
	if err != nil {
		return err
	}
	deleted, err := deleteImages(ctx, client, images)
	if err != nil {
		return err
	}
	if err := confirmTerminated(ctx, client, live, 3*time.Minute, 10*time.Second); err != nil {
		return err
	}
	fmt.Printf("terminated %d of %d environments, deleted %d images, none remain billing\n",
		terminated, len(live), deleted)
	return nil
}

// terminateAll terminates every environment in the region and returns their ids.
func terminateAll(ctx context.Context, client microVMAPI) ([]string, int, error) {
	var live []string
	seen := map[string]struct{}{}
	terminated := 0
	var token *string
	for {
		out, err := client.ListMicrovms(ctx, &lambdamicrovms.ListMicrovmsInput{NextToken: token})
		if err != nil {
			return nil, 0, fmt.Errorf("list environments: %w", err)
		}
		for _, m := range out.Items {
			id := aws.ToString(m.MicrovmId)
			if _, done := seen[id]; done {
				continue
			}
			seen[id] = struct{}{}
			live = append(live, id)
			if m.State == types.MicrovmStateTerminated {
				continue
			}
			if _, err := client.TerminateMicrovm(ctx, &lambdamicrovms.TerminateMicrovmInput{
				MicrovmIdentifier: m.MicrovmId,
			}); err != nil {
				return nil, 0, fmt.Errorf("terminate %s: %w", id, err)
			}
			terminated++
		}
		token = out.NextToken
		if token == nil {
			return live, terminated, nil
		}
	}
}

func deleteImages(ctx context.Context, client microVMAPI, images []string) (int, error) {
	deleted := 0
	for i := range images {
		if _, err := client.DeleteMicrovmImage(ctx, &lambdamicrovms.DeleteMicrovmImageInput{
			ImageIdentifier: &images[i],
		}); err != nil {
			return deleted, fmt.Errorf("delete image %s: %w", images[i], err)
		}
		deleted++
	}
	return deleted, nil
}

// confirmTerminated waits until every environment reports TERMINATED. An
// environment that cannot be read counts as still running.
func confirmTerminated(ctx context.Context, client microVMAPI, live []string, budget, interval time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		var remaining []string
		for i := range live {
			got, err := client.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: &live[i]})
			if err != nil {
				remaining = append(remaining, fmt.Sprintf("%s (unreadable: %v)", live[i], err))
				continue
			}
			if got.State != types.MicrovmStateTerminated {
				remaining = append(remaining, live[i])
			}
		}
		if len(remaining) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d environments are still not terminated: %v", len(remaining), remaining)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
