/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
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
	"log/slog"

	"github.com/gravitational/trace"

	"github.com/gravitational/teleport"
	apiclient "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/lib/tbot/bot"
	"github.com/gravitational/teleport/lib/tbot/config"
	"github.com/gravitational/teleport/lib/tbot/identity"
	"github.com/gravitational/teleport/lib/tbot/internal"
	"github.com/gravitational/teleport/lib/tbot/readyz"
)

func ClientCredentialOutputServiceBuilder(botCfg *config.BotConfig, cfg *config.UnstableClientCredentialOutput) bot.ServiceBuilder {
	return func(deps bot.ServiceDependencies) (bot.Service, error) {
		svc := &ClientCredentialOutputService{
			botAuthClient:      deps.Client,
			botIdentityReadyCh: deps.BotIdentityReadyCh,
			botCfg:             botCfg,
			cfg:                cfg,
			reloadCh:           deps.ReloadCh,
			identityGenerator:  deps.IdentityGenerator,
		}
		svc.log = deps.Logger.With(
			teleport.ComponentKey,
			teleport.Component(teleport.ComponentTBot, "svc", svc.String()),
		)
		svc.statusReporter = deps.StatusRegistry.AddService(svc.String())
		return svc, nil
	}
}

// ClientCredentialOutputService produces credentials which can be used to
// connect to Teleport's API or SSH.
type ClientCredentialOutputService struct {
	// botAuthClient should be an auth client using the bots internal identity.
	// This will not have any roles impersonated and should only be used to
	// fetch CAs.
	botAuthClient      *apiclient.Client
	botIdentityReadyCh <-chan struct{}
	botCfg             *config.BotConfig
	cfg                *config.UnstableClientCredentialOutput
	log                *slog.Logger
	statusReporter     readyz.Reporter
	reloadCh           <-chan struct{}
	identityGenerator  *identity.Generator
}

func (s *ClientCredentialOutputService) String() string {
	return cmp.Or(
		s.cfg.Name,
		"client-credential-output",
	)
}

func (s *ClientCredentialOutputService) OneShot(ctx context.Context) error {
	return s.generate(ctx)
}

func (s *ClientCredentialOutputService) Run(ctx context.Context) error {
	err := internal.RunOnInterval(ctx, internal.RunOnIntervalConfig{
		Service:         s.String(),
		Name:            "output-renewal",
		F:               s.generate,
		Interval:        s.botCfg.CredentialLifetime.RenewalInterval,
		RetryLimit:      internal.RenewalRetryLimit,
		Log:             s.log,
		ReloadCh:        s.reloadCh,
		IdentityReadyCh: s.botIdentityReadyCh,
		StatusReporter:  s.statusReporter,
	})
	return trace.Wrap(err)
}

func (s *ClientCredentialOutputService) generate(ctx context.Context) error {
	ctx, span := tracer.Start(
		ctx,
		"ClientCredentialOutputService/generate",
	)
	defer span.End()
	s.log.InfoContext(ctx, "Generating output")

	id, err := s.identityGenerator.Generate(ctx,
		identity.WithLifetime(s.botCfg.CredentialLifetime.TTL, s.botCfg.CredentialLifetime.RenewalInterval),
		identity.WithLogger(s.log),
	)
	if err != nil {
		return trace.Wrap(err, "generating identity")
	}

	s.cfg.SetOrUpdateFacade(id)
	return nil
}
