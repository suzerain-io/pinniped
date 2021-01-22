// Copyright 2020-2021 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package server is the command line entry point for pinniped-concierge.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"

	loginv1alpha1 "go.pinniped.dev/generated/1.20/apis/concierge/login/v1alpha1"
	"go.pinniped.dev/internal/apigroup"
	"go.pinniped.dev/internal/certauthority"
	"go.pinniped.dev/internal/certauthority/dynamiccertauthority"
	"go.pinniped.dev/internal/concierge/apiserver"
	"go.pinniped.dev/internal/concierge/impersonator"
	"go.pinniped.dev/internal/config/concierge"
	"go.pinniped.dev/internal/controller/authenticator/authncache"
	"go.pinniped.dev/internal/controllermanager"
	"go.pinniped.dev/internal/downward"
	"go.pinniped.dev/internal/dynamiccert"
	"go.pinniped.dev/internal/here"
	"go.pinniped.dev/internal/plog"
	"go.pinniped.dev/internal/registry/credentialrequest"
)

// App is an object that represents the pinniped-concierge application.
type App struct {
	cmd *cobra.Command

	// CLI flags
	configPath      string
	downwardAPIPath string
}

// New constructs a new App with command line args, stdout and stderr.
func New(ctx context.Context, args []string, stdout, stderr io.Writer) *App {
	app := &App{}
	app.addServerCommand(ctx, args, stdout, stderr)
	return app
}

// Run the server.
func (a *App) Run() error {
	return a.cmd.Execute()
}

// Create the server command and save it into the App.
func (a *App) addServerCommand(ctx context.Context, args []string, stdout, stderr io.Writer) {
	cmd := &cobra.Command{
		Use: "pinniped-concierge",
		Long: here.Doc(`
			pinniped-concierge provides a generic API for mapping an external
			credential from somewhere to an internal credential to be used for
			authenticating to the Kubernetes API.`),
		RunE: func(cmd *cobra.Command, args []string) error { return a.runServer(ctx) },
		Args: cobra.NoArgs,
	}

	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	addCommandlineFlagsToCommand(cmd, a)

	a.cmd = cmd
}

// Define the app's commandline flags.
func addCommandlineFlagsToCommand(cmd *cobra.Command, app *App) {
	cmd.Flags().StringVarP(
		&app.configPath,
		"config",
		"c",
		"pinniped.yaml",
		"path to configuration file",
	)

	cmd.Flags().StringVar(
		&app.downwardAPIPath,
		"downward-api-path",
		"/etc/podinfo",
		"path to Downward API volume mount",
	)

	plog.RemoveKlogGlobalFlags()
}

// Boot the aggregated API server, which will in turn boot the controllers.
func (a *App) runServer(ctx context.Context) error {
	// Read the server config file.
	cfg, err := concierge.FromPath(a.configPath)
	if err != nil {
		return fmt.Errorf("could not load config: %w", err)
	}

	// Discover in which namespace we are installed.
	podInfo, err := downward.Load(a.downwardAPIPath)
	if err != nil {
		return fmt.Errorf("could not read pod metadata: %w", err)
	}

	// Initialize the cache of active authenticators.
	authenticators := authncache.New()

	// This cert provider will provide certs to the API server and will
	// be mutated by a controller to keep the certs up to date with what
	// is stored in a k8s Secret. Therefore it also effectively acting as
	// an in-memory cache of what is stored in the k8s Secret, helping to
	// keep incoming requests fast.
	dynamicServingCertProvider := dynamiccert.New()

	// This cert provider will be used to provide a signing key to the
	// cert issuer used to issue certs to Pinniped clients wishing to login.
	dynamicSigningCertProvider := dynamiccert.New()

	// Prepare to start the controllers, but defer actually starting them until the
	// post start hook of the aggregated API server.
	startControllersFunc, err := controllermanager.PrepareControllers(
		&controllermanager.Config{
			ServerInstallationInfo:     podInfo,
			APIGroupSuffix:             *cfg.APIGroupSuffix,
			NamesConfig:                &cfg.NamesConfig,
			Labels:                     cfg.Labels,
			KubeCertAgentConfig:        &cfg.KubeCertAgentConfig,
			DiscoveryURLOverride:       cfg.DiscoveryInfo.URL,
			DynamicServingCertProvider: dynamicServingCertProvider,
			DynamicSigningCertProvider: dynamicSigningCertProvider,
			ServingCertDuration:        time.Duration(*cfg.APIConfig.ServingCertificateConfig.DurationSeconds) * time.Second,
			ServingCertRenewBefore:     time.Duration(*cfg.APIConfig.ServingCertificateConfig.RenewBeforeSeconds) * time.Second,
			AuthenticatorCache:         authenticators,
		},
	)
	if err != nil {
		return fmt.Errorf("could not prepare controllers: %w", err)
	}

	// Get the aggregated API server config.
	aggregatedAPIServerConfig, err := getAggregatedAPIServerConfig(
		dynamicServingCertProvider,
		authenticators,
		dynamiccertauthority.New(dynamicSigningCertProvider),
		startControllersFunc,
		*cfg.APIGroupSuffix,
	)
	if err != nil {
		return fmt.Errorf("could not configure aggregated API server: %w", err)
	}

	// Complete the aggregated API server config and make a server instance.
	server, err := aggregatedAPIServerConfig.Complete().New()
	if err != nil {
		return fmt.Errorf("could not create aggregated API server: %w", err)
	}

	// run proxy handler
	impersonationCA, err := certauthority.New(pkix.Name{CommonName: "test CA"}, 24*time.Hour)
	if err != nil {
		return fmt.Errorf("could not create impersonation CA: %w", err)
	}
	impersonationCert, err := impersonationCA.Issue(pkix.Name{}, []string{"impersonation-proxy"}, nil, 24*time.Hour)
	if err != nil {
		return fmt.Errorf("could not create impersonation cert: %w", err)
	}
	impersonationProxy, err := impersonator.New(authenticators, klogr.New().WithName("impersonation-proxy"))
	if err != nil {
		return fmt.Errorf("could not create impersonation proxy: %w", err)
	}

	impersonationProxyServer := http.Server{
		Addr:    "0.0.0.0:8444",
		Handler: impersonationProxy,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{*impersonationCert},
		},
	}
	go func() {
		if err := impersonationProxyServer.ListenAndServeTLS("", ""); err != nil {
			klog.ErrorS(err, "could not serve impersonation proxy")
		}
	}()

	// Run the server. Its post-start hook will start the controllers.
	return server.GenericAPIServer.PrepareRun().Run(ctx.Done())
}

// Create a configuration for the aggregated API server.
func getAggregatedAPIServerConfig(
	dynamicCertProvider dynamiccert.Provider,
	authenticator credentialrequest.TokenCredentialRequestAuthenticator,
	issuer credentialrequest.CertIssuer,
	startControllersPostStartHook func(context.Context),
	apiGroupSuffix string,
) (*apiserver.Config, error) {
	// This is ignored for now because we turn off etcd storage below, but this is
	// the right prefix in case we turn it back on.
	apiGroup, ok := apigroup.Make(loginv1alpha1.GroupName, apiGroupSuffix)
	if !ok {
		return nil, fmt.Errorf("cannot make api group from %s/%s", loginv1alpha1.GroupName, apiGroupSuffix)
	}
	defaultEtcdPathPrefix := fmt.Sprintf("/registry/%s", apiGroup)

	recommendedOptions := genericoptions.NewRecommendedOptions(
		defaultEtcdPathPrefix,
		apiserver.Codecs.LegacyCodec(loginv1alpha1.SchemeGroupVersion),
		// TODO we should check to see if all the other default settings are acceptable for us
	)
	recommendedOptions.Etcd = nil // turn off etcd storage because we don't need it yet
	recommendedOptions.SecureServing.ServerCert.GeneratedCert = dynamicCertProvider
	recommendedOptions.SecureServing.BindPort = 8443 // Don't run on default 443 because that requires root

	serverConfig := genericapiserver.NewRecommendedConfig(apiserver.Codecs)
	// Note that among other things, this ApplyTo() function copies
	// `recommendedOptions.SecureServing.ServerCert.GeneratedCert` into
	// `serverConfig.SecureServing.Cert` thus making `dynamicCertProvider`
	// the cert provider for the running server. The provider will be called
	// by the API machinery periodically. When the provider returns nil certs,
	// the API server will return "the server is currently unable to
	// handle the request" error responses for all incoming requests.
	// If the provider later starts returning certs, then the API server
	// will use them to handle the incoming requests successfully.
	if err := recommendedOptions.ApplyTo(serverConfig); err != nil {
		return nil, err
	}

	apiServerConfig := &apiserver.Config{
		GenericConfig: serverConfig,
		ExtraConfig: apiserver.ExtraConfig{
			Authenticator:                 authenticator,
			Issuer:                        issuer,
			StartControllersPostStartHook: startControllersPostStartHook,
		},
	}
	return apiServerConfig, nil
}
