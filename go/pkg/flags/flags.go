// Package flags provides a convenient way to initialize the remote client from flags.
package flags

import (
	"context"
	"crypto/x509"
	"flag"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/balancer"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/client"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/moreflag"
)

var (
	// The flags credential_file, use_application_default_credentials, and use_gce_credentials
	// determine the client identity that is used to authenticate with remote execution.
	// One of the following must be true for client authentication to work, and they are used in
	// this order of preference:
	// - the use_application_default_credentials flag must be set to true, or
	// - the use_gce_credentials must be set to true, or
	// - the credential_file flag must be set to point to a valid credential file

	// CredFile is the name of a file that contains service account credentials to use when calling
	// remote execution. Used only if --use_application_default_credentials and --use_gce_credentials
	// are false.
	CredFile = flag.String("credential_file", "", "The name of a file that contains service account credentials to use when calling remote execution. Used only if --use_application_default_credentials and --use_gce_credentials are false.")
	// UseApplicationDefaultCreds is whether to use application default credentials to connect to
	// remote execution. See
	// https://cloud.google.com/sdk/gcloud/reference/auth/application-default/login
	UseApplicationDefaultCreds = flag.Bool("use_application_default_credentials", false, "If true, use application default credentials to connect to remote execution. See https://cloud.google.com/sdk/gcloud/reference/auth/application-default/login")
	// UseGCECredentials is whether to use the default GCE credentials to authenticate with remote
	// execution. --use_application_default_credentials must be false.
	UseGCECredentials = flag.Bool("use_gce_credentials", false, "If true (and --use_application_default_credentials is false), use the default GCE credentials to authenticate with remote execution.")
	// UseRPCCredentials can be set to false to disable all per-RPC credentials.
	UseRPCCredentials = flag.Bool("use_rpc_credentials", true, "If false, no per-RPC credentials will be used (disables --credential_file, --use_application_default_credentials, and --use_gce_credentials.")
	// Service represents the host (and, if applicable, port) of the remote execution service.
	Service = flag.String("service", "", "The remote execution service to dial when calling via gRPC, including port, such as 'localhost:8790' or 'remotebuildexecution.googleapis.com:443'")
	// CASService represents the host (and, if applicable, port) of the CAS service, if different from the remote execution service.
	CASService = flag.String("cas_service", "", "The CAS service to dial when calling via gRPC, including port, such as 'localhost:8790' or 'remotebuildexecution.googleapis.com:443'")
	// Instance gives the instance of remote execution to test (in
	// projects/[PROJECT_ID]/instances/[INSTANCE_NAME] format for Google RBE).
	Instance = flag.String("instance", "", "The instance ID to target when calling remote execution via gRPC (e.g., projects/$PROJECT/instances/default_instance for Google RBE).")
	// CASConcurrency specifies the maximum number of concurrent upload & download RPCs that can be in flight.
	CASConcurrency = flag.Int("cas_concurrency", client.DefaultCASConcurrency, "Num concurrent upload / download RPCs that the SDK is allowed to do.")
	// MaxConcurrentRequests denotes the maximum number of concurrent RPCs on a single gRPC connection.
	MaxConcurrentRequests = flag.Uint("max_concurrent_requests_per_conn", client.DefaultMaxConcurrentRequests, "Maximum number of concurrent RPCs on a single gRPC connection.")
	// MaxConcurrentStreams denotes the maximum number of concurrent stream RPCs on a single gRPC connection.
	MaxConcurrentStreams = flag.Uint("max_concurrent_streams_per_conn", client.DefaultMaxConcurrentStreams, "Maximum number of concurrent stream RPCs on a single gRPC connection.")
	// TLSServerName overrides the server name sent in the TLS session.
	TLSServerName = flag.String("tls_server_name", "", "Override the TLS server name")
	// TLSCACert loads CA certificates from a file
	TLSCACert   = flag.String("tls_ca_cert", "", "Load TLS CA certificates from this file")
	RPCTimeouts map[string]string
)

func init() {
	// MinConnections denotes the minimum number of gRPC sub-connections the gRPC balancer should create during SDK initialization.
	flag.IntVar(&balancer.MinConnections, "min_grpc_connections", balancer.DefaultMinConnections, "Minimum number of gRPC sub-connections the gRPC balancer should create during SDK initialization.")
	// RPCTimeouts stores the per-RPC timeout values. The flag allows users to override the defaults
	// set in client.DefaultRPCTimeouts. This is in order to not force the users to familiarize
	// themselves with every RPC, otherwise it is easy to accidentally enforce a timeout on
	// WaitExecution, for example.
	flag.Var((*moreflag.StringMapValue)(&RPCTimeouts), "rpc_timeouts", "Comma-separated key value pairs in the form rpc_name=timeout. The key for default RPC is named default. 0 indicates no timeout. Example: GetActionResult=500ms,Execute=0,default=10s.")
}

// NewClientFromFlags connects to a remote execution service and returns a client suitable for higher-level
// functionality. It uses the flags from above to configure the connection to remote execution.
func NewClientFromFlags(ctx context.Context, opts ...client.Opt) (*client.Client, error) {
	opts = append(opts, client.CASConcurrency(*CASConcurrency))
	if len(RPCTimeouts) > 0 {
		timeouts := make(map[string]time.Duration)
		for rpc, d := range client.DefaultRPCTimeouts {
			timeouts[rpc] = d
		}
		// Override the defaults with flags, but do not replace.
		for rpc, s := range RPCTimeouts {
			d, err := time.ParseDuration(s)
			if err != nil {
				return nil, err
			}
			timeouts[rpc] = d
		}
		opts = append(opts, client.RPCTimeouts(timeouts))
	}
	certPool := x509.NewCertPool()
	if *TLSCACert != "" {
		ca, err := ioutil.ReadFile(*TLSCACert)
		if err != nil {
			return nil, err
		}
		if ok := certPool.AppendCertsFromPEM(ca); !ok {
			return nil, fmt.Errorf("failed to load TLS CA certificates from %s", *TLSCACert)
		}
	}
	return client.NewClient(ctx, *Instance, client.DialParams{
		Service:               *Service,
		CASService:            *CASService,
		CredFile:              *CredFile,
		UseApplicationDefault: *UseApplicationDefaultCreds,
		UseComputeEngine:      *UseGCECredentials,
		TransportCredsOnly:    !*UseRPCCredentials,
		TLSServerName:         *TLSServerName,
		CertPool:              certPool,
		MaxConcurrentRequests: uint32(*MaxConcurrentRequests),
		MaxConcurrentStreams:  uint32(*MaxConcurrentStreams),
	}, opts...)
}
