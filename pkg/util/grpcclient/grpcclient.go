package grpcclient

import (
	"flag"
	"time"

	"github.com/go-kit/log"
	middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	grpcbackoff "google.golang.org/grpc/backoff"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/keepalive"

	"github.com/cortexproject/cortex/pkg/util/backoff"
	"github.com/cortexproject/cortex/pkg/util/grpcencoding/snappy"
	"github.com/cortexproject/cortex/pkg/util/grpcencoding/snappyblock"
	"github.com/cortexproject/cortex/pkg/util/grpcencoding/zstd"
	"github.com/cortexproject/cortex/pkg/util/tls"
)

// Config for a gRPC client.
type Config struct {
	MaxRecvMsgSize  int     `yaml:"max_recv_msg_size"`
	MaxSendMsgSize  int     `yaml:"max_send_msg_size"`
	GRPCCompression string  `yaml:"grpc_compression"`
	RateLimit       float64 `yaml:"rate_limit"`
	RateLimitBurst  int     `yaml:"rate_limit_burst"`

	BackoffOnRatelimits bool           `yaml:"backoff_on_ratelimits"`
	BackoffConfig       backoff.Config `yaml:"backoff_config"`

	TLSEnabled               bool             `yaml:"tls_enabled"`
	TLS                      tls.ClientConfig `yaml:",inline"`
	SignWriteRequestsEnabled bool             `yaml:"-"`

	ConnectTimeout time.Duration `yaml:"connect_timeout"`
}

type ConfigWithHealthCheck struct {
	Config            `yaml:",inline"`
	HealthCheckConfig HealthCheckConfig `yaml:"healthcheck_config" doc:"description=EXPERIMENTAL: If enabled, gRPC clients perform health checks for each target and fail the request if the target is marked as unhealthy."`
}

// RegisterFlags registers flags.
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	cfg.RegisterFlagsWithPrefix("", "", f)
}

func (cfg *ConfigWithHealthCheck) RegisterFlagsWithPrefix(prefix, defaultGrpcCompression string, f *flag.FlagSet) {
	cfg.Config.RegisterFlagsWithPrefix(prefix, defaultGrpcCompression, f)
	cfg.HealthCheckConfig.RegisterFlagsWithPrefix(prefix, f)
}

// RegisterFlagsWithPrefix registers flags with prefix.
func (cfg *Config) RegisterFlagsWithPrefix(prefix, defaultGrpcCompression string, f *flag.FlagSet) {
	f.IntVar(&cfg.MaxRecvMsgSize, prefix+".grpc-max-recv-msg-size", 100<<20, "gRPC client max receive message size (bytes).")
	f.IntVar(&cfg.MaxSendMsgSize, prefix+".grpc-max-send-msg-size", 16<<20, "gRPC client max send message size (bytes).")
	f.StringVar(&cfg.GRPCCompression, prefix+".grpc-compression", defaultGrpcCompression, "Use compression when sending messages. Supported values are: 'gzip', 'snappy', 'snappy-block' ,'zstd' and '' (disable compression)")
	f.Float64Var(&cfg.RateLimit, prefix+".grpc-client-rate-limit", 0., "Rate limit for gRPC client; 0 means disabled.")
	f.IntVar(&cfg.RateLimitBurst, prefix+".grpc-client-rate-limit-burst", 0, "Rate limit burst for gRPC client.")
	f.BoolVar(&cfg.BackoffOnRatelimits, prefix+".backoff-on-ratelimits", false, "Enable backoff and retry when we hit ratelimits.")
	f.BoolVar(&cfg.TLSEnabled, prefix+".tls-enabled", cfg.TLSEnabled, "Enable TLS in the GRPC client. This flag needs to be enabled when any other TLS flag is set. If set to false, insecure connection to gRPC server will be used.")
	f.DurationVar(&cfg.ConnectTimeout, prefix+".connect-timeout", 5*time.Second, "The maximum amount of time to establish a connection. A value of 0 means using default gRPC client connect timeout 20s.")

	cfg.BackoffConfig.RegisterFlagsWithPrefix(prefix, f)

	cfg.TLS.RegisterFlagsWithPrefix(prefix, f)
}

func (cfg *Config) Validate(log log.Logger) error {
	switch cfg.GRPCCompression {
	case gzip.Name, snappy.Name, zstd.Name, snappyblock.Name, "":
		// valid
	default:
		return errors.Errorf("unsupported compression type: %s", cfg.GRPCCompression)
	}
	return nil
}

// CallOptions returns the config in terms of CallOptions.
func (cfg *Config) CallOptions() []grpc.CallOption {
	var opts []grpc.CallOption
	opts = append(opts, grpc.MaxCallRecvMsgSize(cfg.MaxRecvMsgSize))
	opts = append(opts, grpc.MaxCallSendMsgSize(cfg.MaxSendMsgSize))
	if cfg.GRPCCompression != "" {
		opts = append(opts, grpc.UseCompressor(cfg.GRPCCompression))
	}
	return opts
}

func (cfg *ConfigWithHealthCheck) DialOption(unaryClientInterceptors []grpc.UnaryClientInterceptor, streamClientInterceptors []grpc.StreamClientInterceptor) ([]grpc.DialOption, error) {
	if cfg.HealthCheckConfig.HealthCheckInterceptors != nil {
		unaryClientInterceptors = append(unaryClientInterceptors, cfg.HealthCheckConfig.UnaryHealthCheckInterceptor(cfg))
		streamClientInterceptors = append(streamClientInterceptors, cfg.HealthCheckConfig.StreamClientInterceptor(cfg))
	}

	return cfg.Config.DialOption(unaryClientInterceptors, streamClientInterceptors)
}

// DialOption returns the config as a grpc.DialOptions.
func (cfg *Config) DialOption(unaryClientInterceptors []grpc.UnaryClientInterceptor, streamClientInterceptors []grpc.StreamClientInterceptor) ([]grpc.DialOption, error) {
	var opts []grpc.DialOption
	tlsOpts, err := cfg.TLS.GetGRPCDialOptions(cfg.TLSEnabled)
	if err != nil {
		return nil, err
	}
	opts = append(opts, tlsOpts...)

	if cfg.BackoffOnRatelimits {
		unaryClientInterceptors = append([]grpc.UnaryClientInterceptor{NewBackoffRetry(cfg.BackoffConfig)}, unaryClientInterceptors...)
	}

	if cfg.RateLimit > 0 {
		unaryClientInterceptors = append([]grpc.UnaryClientInterceptor{NewRateLimiter(cfg)}, unaryClientInterceptors...)
	}

	if cfg.ConnectTimeout > 0 {
		opts = append(
			opts,
			grpc.WithConnectParams(grpc.ConnectParams{
				Backoff:           grpcbackoff.DefaultConfig,
				MinConnectTimeout: cfg.ConnectTimeout,
			}),
		)
	}

	if cfg.SignWriteRequestsEnabled {
		unaryClientInterceptors = append(unaryClientInterceptors, UnarySigningClientInterceptor)
	}

	return append(
		opts,
		grpc.WithDefaultCallOptions(cfg.CallOptions()...),
		grpc.WithUnaryInterceptor(middleware.ChainUnaryClient(unaryClientInterceptors...)),
		grpc.WithStreamInterceptor(middleware.ChainStreamClient(streamClientInterceptors...)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                time.Second * 20,
			Timeout:             time.Second * 10,
			PermitWithoutStream: true,
		}),
	), nil
}
