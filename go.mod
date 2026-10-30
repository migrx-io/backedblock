module github.com/migrx-io/backedblock

go 1.26.0

require (
	github.com/aws/aws-sdk-go-v2 v1.47.5
	github.com/aws/aws-sdk-go-v2/config v1.33.12
	github.com/aws/aws-sdk-go-v2/service/ec2 v1.343.0
	github.com/aws/aws-sdk-go-v2/service/s3 v1.114.6
	github.com/aws/aws-sdk-go-v2/service/ssm v1.81.1
	github.com/aws/aws-sdk-go-v2/service/sts v1.51.6
	github.com/aws/session-manager-plugin v0.0.0-20260615221425-930a08e65d3a
	github.com/aws/smithy-go v1.28.7
	github.com/hashicorp/go-version v1.9.0
	github.com/hashicorp/hc-install v0.10.0
	github.com/hashicorp/terraform-exec v0.26.0
	github.com/hashicorp/terraform-json v0.29.0
	github.com/spf13/cobra v1.10.2
	golang.org/x/crypto v0.58.0
	golang.org/x/term v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/ProtonMail/go-crypto v1.5.2 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.25 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.20.12 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.20.5 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.8 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.8 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/kms v1.62.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.12.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.39.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.44.0 // indirect
	github.com/cihub/seelog v0.0.0-20170130134532-f561c5e57575 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-retryablehttp v0.7.8 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/stretchr/objx v0.5.3 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	// Pinned: session-manager-plugin needs the pre-v1 twinj/uuid API (uuid.UUID as
	// an interface, uuid.CleanHyphen); v1.0.0 breaks its build. Do not bump.
	github.com/twinj/uuid v0.0.0-20151029044442-89173bcdda19 // indirect
	github.com/xtaci/smux v1.5.58 // indirect
	github.com/zclconf/go-cty v1.18.1 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)
