/*
 * Teleport
 * Copyright (C) 2024  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package tbot

import (
	"cmp"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"

	"github.com/gravitational/trace"

	"github.com/gravitational/teleport"
	apiclient "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/lib/srv/alpnproxy"
	"github.com/gravitational/teleport/lib/srv/alpnproxy/common"
	"github.com/gravitational/teleport/lib/tbot/bot"
	"github.com/gravitational/teleport/lib/tbot/bot/connection"
	"github.com/gravitational/teleport/lib/tbot/client"
	"github.com/gravitational/teleport/lib/tbot/config"
	"github.com/gravitational/teleport/lib/tbot/identity"
	"github.com/gravitational/teleport/lib/tbot/internal"
	"github.com/gravitational/teleport/lib/tbot/readyz"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"
)

func DatabaseTunnelServiceBuilder(botCfg *config.BotConfig, cfg *config.DatabaseTunnelService) bot.ServiceBuilder {
	return func(deps bot.ServiceDependencies) (bot.Service, error) {
		svc := &DatabaseTunnelService{
			botCfg:             botCfg,
			cfg:                cfg,
			proxyPinger:        deps.ProxyPinger,
			botClient:          deps.Client,
			getBotIdentity:     deps.BotIdentity,
			botIdentityReadyCh: deps.BotIdentityReadyCh,
			identityGenerator:  deps.IdentityGenerator,
			clientBuilder:      deps.ClientBuilder,
		}
		svc.log = deps.Logger.With(
			teleport.ComponentKey,
			teleport.Component(teleport.ComponentTBot, "svc", svc.String()),
		)
		svc.statusReporter = deps.StatusRegistry.AddService(svc.String())
		return svc, nil
	}
}

// DatabaseTunnelService is a service that listens on a local port and forwards
// connections to a remote database service. It is an authenticating tunnel and
// will automatically issue and renew certificates as needed.
type DatabaseTunnelService struct {
	botCfg             *config.BotConfig
	cfg                *config.DatabaseTunnelService
	proxyPinger        connection.ProxyPinger
	log                *slog.Logger
	botClient          *apiclient.Client
	getBotIdentity     getBotIdentityFn
	botIdentityReadyCh <-chan struct{}
	statusReporter     readyz.Reporter
	identityGenerator  *identity.Generator
	clientBuilder      *client.Builder
}

// buildLocalProxyConfig initializes the service, fetching any initial information and setting
// up the localproxy.
func (s *DatabaseTunnelService) buildLocalProxyConfig(ctx context.Context) (lpCfg alpnproxy.LocalProxyConfig, err error) {
	ctx, span := tracer.Start(ctx, "DatabaseTunnelService/buildLocalProxyConfig")
	defer span.End()

	if s.botIdentityReadyCh != nil {
		select {
		case <-s.botIdentityReadyCh:
		default:
			s.log.InfoContext(ctx, "Waiting for internal bot identity to be renewed before running")
			select {
			case <-s.botIdentityReadyCh:
			case <-ctx.Done():
				return alpnproxy.LocalProxyConfig{}, ctx.Err()
			}
		}
	}

	proxyPing, err := s.proxyPinger.Ping(ctx)
	if err != nil {
		return alpnproxy.LocalProxyConfig{}, trace.Wrap(err, "pinging proxy")
	}
	proxyAddr, err := proxyPing.ProxyWebAddr()
	if err != nil {
		return alpnproxy.LocalProxyConfig{}, trace.Wrap(err, "determining proxy web address")
	}

	// Fetch information about the database and then issue the initial
	// certificate. We issue the initial certificate to allow us to fail faster.
	// We cache the routeToDatabase as these will not change during the lifetime
	// of the service and this reduces the time needed to issue a new
	// certificate.
	s.log.DebugContext(ctx, "Determining route to database.")
	routeToDatabase, err := s.getRouteToDatabaseWithImpersonation(ctx)
	if err != nil {
		return alpnproxy.LocalProxyConfig{}, trace.Wrap(err)
	}
	s.log.DebugContext(
		ctx,
		"Identified route to database.",
		"service_name", routeToDatabase.ServiceName,
		"protocol", routeToDatabase.Protocol,
		"database", routeToDatabase.Database,
		"username", routeToDatabase.Username,
	)

	s.log.DebugContext(ctx, "Issuing initial certificate for local proxy.")
	dbCert, err := s.issueCert(ctx, routeToDatabase)
	if err != nil {
		return alpnproxy.LocalProxyConfig{}, trace.Wrap(err)
	}
	s.log.DebugContext(ctx, "Issued initial certificate for local proxy.")

	middleware := internal.ALPNProxyMiddleware{
		OnNewConnectionFunc: func(ctx context.Context, lp *alpnproxy.LocalProxy) error {
			ctx, span := tracer.Start(ctx, "DatabaseTunnelService/OnNewConnection")
			defer span.End()

			// Check if the certificate needs reissuing, if so, reissue.
			if err := lp.CheckDBCert(ctx, tlsca.RouteToDatabase{
				ServiceName: routeToDatabase.ServiceName,
				Protocol:    routeToDatabase.Protocol,
				Database:    routeToDatabase.Database,
				Username:    routeToDatabase.Username,
			}); err != nil {
				s.log.InfoContext(ctx, "Certificate for tunnel needs reissuing.", "reason", err.Error())
				cert, err := s.issueCert(ctx, routeToDatabase)
				if err != nil {
					return trace.Wrap(err, "issuing cert")
				}
				lp.SetCert(*cert)
			}
			return nil
		},
	}

	alpnProtocol, err := common.ToALPNProtocol(routeToDatabase.Protocol)
	if err != nil {
		return alpnproxy.LocalProxyConfig{}, trace.Wrap(err)

	}
	lpConfig := alpnproxy.LocalProxyConfig{
		Middleware: middleware,

		RemoteProxyAddr:    proxyAddr,
		ParentContext:      ctx,
		Protocols:          []common.Protocol{alpnProtocol},
		Cert:               *dbCert,
		InsecureSkipVerify: s.botCfg.Insecure,
	}
	if apiclient.IsALPNConnUpgradeRequired(
		ctx,
		proxyAddr,
		s.botCfg.Insecure,
	) {
		lpConfig.ALPNConnUpgradeRequired = true
		// If ALPN Conn Upgrade will be used, we need to set the cluster CAs
		// to validate the Proxy's auth issued host cert.
		lpConfig.RootCAs = s.getBotIdentity().TLSCAPool
	}

	return lpConfig, nil
}

func (s *DatabaseTunnelService) Run(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "DatabaseTunnelService/Run")
	defer span.End()

	l := s.cfg.Listener
	if l == nil {
		s.log.DebugContext(ctx, "Opening listener for database tunnel.", "listen", s.cfg.Listen)
		var err error
		l, err = createListener(ctx, s.log, s.cfg.Listen)
		if err != nil {
			return trace.Wrap(err, "opening listener")
		}
		defer func() {
			if err := l.Close(); err != nil && !utils.IsUseOfClosedNetworkError(err) {
				s.log.ErrorContext(ctx, "Failed to close listener", "error", err)
			}
		}()
	}

	lpCfg, err := s.buildLocalProxyConfig(ctx)
	if err != nil {
		return trace.Wrap(err, "building local proxy config")
	}
	lpCfg.Listener = l

	lp, err := alpnproxy.NewLocalProxy(lpCfg)
	if err != nil {
		return trace.Wrap(err, "creating local proxy")
	}
	defer func() {
		if err := lp.Close(); err != nil {
			s.log.ErrorContext(ctx, "Failed to close local proxy", "error", err)
		}
	}()
	// Closed further down.

	// lp.Start will block and continues to block until lp.Close() is called.
	// Despite taking a context, it will not exit until the first connection is
	// made after the context is canceled.
	var errCh = make(chan error, 1)
	go func() {
		errCh <- lp.Start(ctx)
	}()
	s.log.InfoContext(ctx, "Listening for connections.", "address", l.Addr().String())

	s.statusReporter.Report(readyz.Healthy)

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		s.statusReporter.ReportReason(readyz.Unhealthy, err.Error())
		return trace.Wrap(err, "local proxy failed")
	}
}

// getRouteToDatabaseWithImpersonation fetches the route to the database with
// impersonation of roles. This ensures that the user's selected roles actually
// grant access to the database.
func (s *DatabaseTunnelService) getRouteToDatabaseWithImpersonation(ctx context.Context) (proto.RouteToDatabase, error) {
	ctx, span := tracer.Start(ctx, "DatabaseTunnelService/getRouteToDatabaseWithImpersonation")
	defer span.End()

	effectiveLifetime := cmp.Or(s.cfg.CredentialLifetime, s.botCfg.CredentialLifetime)
	impersonatedIdentity, err := s.identityGenerator.GenerateFacade(ctx,
		identity.WithRoles(s.cfg.Roles),
		identity.WithLifetime(effectiveLifetime.TTL, effectiveLifetime.RenewalInterval),
		identity.WithLogger(s.log),
	)
	if err != nil {
		return proto.RouteToDatabase{}, trace.Wrap(err)
	}

	impersonatedClient, err := s.clientBuilder.Build(ctx, impersonatedIdentity)
	if err != nil {
		return proto.RouteToDatabase{}, trace.Wrap(err)
	}
	defer func() {
		if err := impersonatedClient.Close(); err != nil {
			s.log.ErrorContext(ctx, "Failed to close impersonated client.", "error", err)
		}
	}()

	return getRouteToDatabase(ctx, s.log, impersonatedClient, s.cfg.Service, s.cfg.Username, s.cfg.Database)
}

func (s *DatabaseTunnelService) issueCert(
	ctx context.Context,
	route proto.RouteToDatabase,
) (*tls.Certificate, error) {
	ctx, span := tracer.Start(ctx, "DatabaseTunnelService/issueCert")
	defer span.End()

	s.log.DebugContext(ctx, "Requesting issuance of certificate for tunnel proxy.")
	effectiveLifetime := cmp.Or(s.cfg.CredentialLifetime, s.botCfg.CredentialLifetime)
	ident, err := s.identityGenerator.Generate(ctx,
		identity.WithRoles(s.cfg.Roles),
		identity.WithLifetime(effectiveLifetime.TTL, effectiveLifetime.RenewalInterval),
		identity.WithLogger(s.log),
		identity.WithRouteToDatabase(route),
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	s.log.InfoContext(ctx, "Certificate issued for tunnel proxy.")

	return ident.TLSCert, nil
}

// String returns a human-readable string that can uniquely identify the
// service.
func (s *DatabaseTunnelService) String() string {
	return cmp.Or(
		s.cfg.Name,
		fmt.Sprintf("%s:%s:%s", config.DatabaseTunnelServiceType, s.cfg.Listen, s.cfg.Service),
	)
}
