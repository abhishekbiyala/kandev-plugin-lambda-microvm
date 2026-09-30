# AWS Lambda MicroVM executor for Kandev

A Kandev remote executor provider that runs each agent session in its own
[AWS Lambda MicroVM](https://docs.aws.amazon.com/lambda/latest/dg/microvm.html).
Kandev reaches agentctl inside the MicroVM directly over the platform's
authenticated HTTPS endpoint.

## Platform constraints

- **8-hour lifetime.** A MicroVM ends 8 hours after launch, running or suspended.
  Kandev shows the expiry up front.
- **arm64 only.** The image and the agentctl inside it are linux/arm64.
- **agentctl is baked into the image.** A MicroVM filesystem is a memory snapshot,
  so rebuild the image whenever you upgrade Kandev.
- **Cold start.** The first agent start in a new MicroVM reads the agent from a
  lazily loaded snapshot and takes 10-20 seconds; later starts take about a second.
- **No tags on MicroVMs.** The plugin tracks the environments it launched in its
  own data directory. Organizations that require tags on billable resources
  cannot meet that policy with this provider.

## Setup

1. **Build the image context** (needs a sibling `../kandev` checkout at the Kandev
   version you run). List the agents to pre-install as npm `package@version`,
   using the versions your Kandev pins in
   `apps/backend/internal/agent/agents/managed_npm_runtime_versions.json`; an
   agent that is not pre-installed is downloaded at every session start.

   ```sh
   make build-image-artifacts AGENTS="opencode-ai@1.18.32"
   aws s3 cp build/image.zip s3://<bucket>/image.zip
   ```

2. **Build the image.** The build role must trust `lambda.amazonaws.com` and allow
   `s3:GetObject` on the artifact plus CloudWatch Logs writes. The AWS CLI does not
   include this service, so use the bundled command:

   ```sh
   make build-image REGION=<region> NAME=<name> ROLE=<role-arn> ARTIFACT=s3://<bucket>/image.zip
   ```

3. **Package and install** the plugin: `make package`, then install
   `dist/*.tar.gz` in Kandev.

4. **Make Kandev reachable from AWS.** Set `githubCredentialBroker.publicBaseUrl`
   (or `KANDEV_GITHUB_CREDENTIAL_BROKER_PUBLIC_BASE_URL`) to an HTTPS URL of your
   Kandev that the MicroVM can reach. The agent calls back to Kandev through it.

5. **Create an executor profile** in Settings > Executors with the region, the
   image ARN, and a credential source (below). Select the agent credentials to
   copy into the environment in the profile's Remote Credentials section.

The task repository is cloned to `/workspace/<repository>-<branch>`.

## AWS credentials

| Source | Uses | Suited to |
| --- | --- | --- |
| `host` | The AWS SDK default chain of the machine running Kandev: environment variables, the default profile, or its EC2, ECS, or EKS role | Kandev hosted on AWS |
| `profile` | A named profile from the shared AWS config, including SSO and `credential_process` | A desktop install |
| `static` | Access keys stored in the executor profile | Local testing |

`host` and `profile` store no secrets; the plugin resolves them again on every
call, so rotated and short-lived credentials keep working. Static keys with a
session token stop working when the token expires, after which Kandev can no
longer manage the environments they launched.

Set **Role to assume** (and **External ID** if the role requires one) to have the
plugin assume a dedicated role with the credentials above. Anyone who can create
an executor profile acts as that identity, so restrict executor profiles to
administrators when using `host` or `profile`.

The identity needs:

```json
{
  "Effect": "Allow",
  "Action": [
    "lambda:RunMicrovm", "lambda:GetMicrovm", "lambda:TerminateMicrovm",
    "lambda:CreateMicrovmAuthToken", "lambda:PassNetworkConnector"
  ],
  "Resource": "*"
}
```

plus `iam:PassRole` on the environment role, if one is set.

## Coding agent credentials

- **Amazon Bedrock, no keys (recommended on AWS).** Set **Environment role** to an
  IAM role that trusts `lambda.amazonaws.com` and allows `bedrock:InvokeModel` and
  `bedrock:InvokeModelWithResponseStream`. Processes in the MicroVM read the role's
  credentials from instance metadata (IMDSv2), so an agent configured for Bedrock
  (for example OpenCode with an `amazon-bedrock/...` model) needs no API key.
  `AWS_REGION` is already set inside the environment.
- **API keys.** Environment variables on the Kandev agent profile, such as
  `ANTHROPIC_API_KEY`, are passed to the agent.
- **CLI login files** (for example OpenCode's `auth.json`) are copied when selected
  in the profile's Remote Credentials section.

## Development

```sh
make check        # gofmt, vet, tests
make package      # installable tarball
```

`make verify-live REGION=<region> IMAGE=<image-arn>` runs the provider against a
real account with the `host` credential source (see `go run ./cmd/verify-live -h`
for the other sources, a role to assume, and an environment role). It launches a MicroVM, reaches
agentctl through the leases as Kandev does (handshake, instance creation, instance
health), destroys it, and fails if anything it launched is still running.
`make teardown REGION=<region>` terminates every MicroVM in a region.

`-exec 'command'` runs a shell command inside the environment, which is useful for
checking an image.

`go.mod` resolves the Kandev SDK from a sibling `../kandev` checkout, like the
plugin template.

## Requirements

A Kandev release that includes
[kdlbs/kandev#4068](https://github.com/kdlbs/kandev/pull/4068) (agentctl session
instances) and [kdlbs/kandev#4099](https://github.com/kdlbs/kandev/pull/4099)
(public API URL, agent credentials, repository clone, rollback cleanup).
