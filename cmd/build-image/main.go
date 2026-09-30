// Command build-image builds a MicroVM image from an uploaded artifact, using the
// ambient AWS credentials. The AWS CLI does not include the lambda-microvms service.
//
// Usage: go run ./cmd/build-image -region <region> -name <name> -role <build-role-arn> -artifact s3://<bucket>/<key>
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/abhishekbiyala/kandev-plugin-lambda-microvm/internal/awsmicrovm"
)

func main() {
	region := flag.String("region", "", "AWS region")
	name := flag.String("name", "", "image name")
	role := flag.String("role", "", "IAM role the platform assumes to read the artifact")
	artifact := flag.String("artifact", "", "S3 URI of the build artifact zip")
	flag.Parse()
	if *region == "" || *name == "" || *role == "" || *artifact == "" {
		flag.Usage()
		os.Exit(2)
	}
	arn, err := build(context.Background(), *region, *name, *role, *artifact)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build image:", err)
		os.Exit(1)
	}
	fmt.Println(arn)
}

func build(ctx context.Context, region, name, role, artifact string) (string, error) {
	service, err := awsmicrovm.New(ctx, region, awsmicrovm.Credentials{Source: awsmicrovm.SourceHost})
	if err != nil {
		return "", err
	}
	return service.CreateImage(ctx, name, role, artifact)
}
