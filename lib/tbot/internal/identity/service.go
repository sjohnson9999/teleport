/*
 * Teleport
 * Copyright (C) 2025  Gravitational, Inc.
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

package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/gravitational/trace"
	"go.opentelemetry.io/otel"
	"golang.org/x/crypto/ssh"

	"github.com/gravitational/teleport"
	apiclient "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/utils/keys"
	"github.com/gravitational/teleport/api/utils/retryutils"
	"github.com/gravitational/teleport/lib/auth/join"
	"github.com/gravitational/teleport/lib/auth/join/boundkeypair"
	"github.com/gravitational/teleport/lib/auth/state"
	libclient "github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/cryptosuites"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/tbot/bot/connection"
	"github.com/gravitational/teleport/lib/tbot/bot/destination"
	"github.com/gravitational/teleport/lib/tbot/bot/onboarding"
	"github.com/gravitational/teleport/lib/tbot/client"
	"github.com/gravitational/teleport/lib/tbot/identity"
	"github.com/gravitational/teleport/lib/tbot/internal"
	"github.com/gravitational/teleport/lib/tbot/readyz"
	"github.com/gravitational/teleport/lib/utils"
)

var tracer = otel.Tracer("github.com/gravitational/teleport/lib/tbot/internal/identity")

// Config contains configuration and dependencies for the bot identity service.
type Config struct {
	Connection  connection.Config
	Onboarding  onboarding.Config
	Destination destination.Destination

	TTL             time.Duration
	RenewalInterval time.Duration

	FIPS bool

	Logger         *slog.Logger
	ReloadCh       <-chan struct{}
	ClientBuilder  *client.Builder
	StatusReporter readyz.Reporter
}

func (cfg *Config) CheckAndSetDefaults() error {
	if err := cfg.Connection.Validate(); err != nil {
		return trace.Wrap(err)
	}
	if cfg.TTL <= 0 {
		return trace.BadParameter("TTL is required")
	}
	if cfg.RenewalInterval <= 0 {
		return trace.BadParameter("RenewalInterval is required")
	}
	if cfg.ClientBuilder == nil {
		return trace.BadParameter("ClientBuilder is required")
	}
	if cfg.Destination == nil {
		cfg.Destination = destination.NewMemory()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.StatusReporter == nil {
		cfg.StatusReporter = readyz.NoopReporter()
	}
	return nil
}

// NewService creates a new bot identity service.
func NewService(cfg Config) (*Service, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	return &Service{
		cfg:           cfg,
		log:           cfg.Logger,
		clientBuilder: cfg.ClientBuilder,
	}, nil
}

// botIdentityRenewalRetryLimit is the number of permissible consecutive
// failures in renewing the bot identity before the loop exits fatally.
const botIdentityRenewalRetryLimit = 7

// Service is a [bot.Service] that handles renewing the bot's identity.
// It renews the bot's identity periodically and when receiving a broadcasted
// reload signal.
type Service struct {
	log           *slog.Logger
	cfg           Config
	clientBuilder *client.Builder

	mu              sync.Mutex
	client          *apiclient.Client
	facade          *identity.Facade
	initialized     chan struct{}
	initializedOnce sync.Once
}

// GetIdentity returns the current Bot identity.
func (s *Service) GetIdentity() *identity.Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.facade.Get()
}

// GetClient returns the facaded client for the Bot identity for use by other
// components of `tbot`. Consumers should not call `Close` on the client.
func (s *Service) GetClient() *apiclient.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *Service) GetGenerator() (*identity.Generator, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return identity.NewGenerator(identity.GeneratorConfig{
		Client:      s.client,
		BotIdentity: s.facade,
		FIPS:        s.cfg.FIPS,
		Insecure:    s.cfg.Connection.Insecure,
		Logger: s.log.With(
			teleport.ComponentKey,
			teleport.Component(teleport.ComponentTBot, "identity-generator"),
		),
	})
}

// Ready returns a channel that will be closed when the initial identity renewal
// process has completed. It provides a way to "block" startup of services that
// cannot gracefully handle the API client being unavailable.
func (s *Service) Ready() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.initialized == nil {
		s.initialized = make(chan struct{})
	}

	return s.initialized
}

// IsReady returns whether the initial identity renewal process has completed.
func (s *Service) IsReady() bool {
	select {
	case <-s.Ready():
		return true
	default:
		return false
	}
}

// String returns a human-readable name of the service.
func (s *Service) String() string {
	return "identity"
}

func hasTokenChanged(configTokenBytes, identityBytes []byte) bool {
	if len(configTokenBytes) == 0 || len(identityBytes) == 0 {
		return false
	}

	return !bytes.Equal(identityBytes, configTokenBytes)
}

// loadIdentityFromStore attempts to load a persisted identity from a store.
//
// If the persisted identity does not match the onboarding profile/join token,
// a nil identity will be returned. If the identity certificate has expired, the
// bool return value will be false.
func (s *Service) loadIdentityFromStore(ctx context.Context, store destination.Destination) (*identity.Identity, bool) {
	ctx, span := tracer.Start(ctx, "identityService/loadIdentityFromStore")
	defer span.End()
	s.log.InfoContext(ctx, "Loading existing bot identity from store", "store", store)

	loadedIdent, err := identity.LoadIdentity(ctx, store, identity.BotKinds()...)
	if err != nil {
		if trace.IsNotFound(err) {
			s.log.InfoContext(ctx, "No existing bot identity found in store")
			return nil, false
		} else {
			s.log.WarnContext(
				ctx,
				"Failed to load existing bot identity from store",
				"error", err,
			)
			return nil, false
		}
	}

	// Determine if the token configured in the onboarding matches the
	// one used to produce the credentials loaded from disk.
	if s.cfg.Onboarding.HasToken() {
		if token, err := s.cfg.Onboarding.Token(); err == nil {
			sha := sha256.Sum256([]byte(token))
			configTokenHashBytes := []byte(hex.EncodeToString(sha[:]))
			if hasTokenChanged(loadedIdent.TokenHashBytes, configTokenHashBytes) {
				s.log.InfoContext(ctx, "Bot identity loaded from store does not match configured token")
				// If the token has changed, do not return the loaded
				// identity.
				return nil, false
			}
		} else {
			// we failed to get the newly configured token to compare to,
			// we'll assume the last good credentials written to disk should
			// still be used.
			s.log.WarnContext(
				ctx,
				"There was an error loading the configured token to compare to existing identity. Identity loaded from store will be tried",
				"error", err,
			)
		}
	}

	s.log.InfoContext(
		ctx,
		"Loaded existing bot identity from store",
		"identity", loadedIdent,
	)

	now := time.Now().UTC()
	valid := true
	if now.After(loadedIdent.X509Cert.NotAfter) {
		valid = false
		s.log.WarnContext(
			ctx,
			"Identity loaded from store is expired, it will not be used",
			"not_after", loadedIdent.X509Cert.NotAfter.Format(time.RFC3339),
			"current_time", now.Format(time.RFC3339),
		)
	} else if now.Before(loadedIdent.X509Cert.NotBefore) {
		valid = false
		s.log.WarnContext(
			ctx,
			"Identity loaded from store is not yet valid, it will not be used. Confirm that the system time is correct",
			"not_before", loadedIdent.X509Cert.NotBefore.Format(time.RFC3339),
			"current_time", now.Format(time.RFC3339),
		)
	}

	return loadedIdent, valid
}

// Initialize sets up the bot identity at startup. This process has a few
// steps to it.
//
// First, we attempt to load an existing identity from the configured storage.
// This is ignored if we know that the onboarding settings have changed.
//
// If the identity is found, and seems valid, we attempt to renew using this
// identity to give us a fresh set of certificates.
//
// If there is no identity, or the identity is invalid, we'll join using the
// configured onboarding settings.
func (s *Service) Initialize(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "identityService/Initialize")
	defer span.End()

	s.log.InfoContext(ctx, "Initializing bot identity")
	loadedIdent, valid := s.loadIdentityFromStore(ctx, s.cfg.Destination)
	if !valid {
		if !s.cfg.Onboarding.HasToken() {
			// If there's no pre-existing identity (or it has expired) and the
			// configuration contains no join token, we cannot do anything.
			return trace.BadParameter(
				"no existing identity found on disk or join token configured",
			)
		}
		s.log.InfoContext(
			ctx,
			"Bot was unable to load a valid existing identity from the store, will attempt to join using configured token",
		)
	}

	var (
		newIdentity *identity.Identity
		err         error
	)
	if loadedIdent == nil {
		// If there was no identity already on-disk, or it did not match the
		// onboarding profile / join token, try to join from scratch.
		//
		// If this fails, tbot will exit because we cannot proceed with no
		// identity at all.
		if newIdentity, err = botIdentityFromToken(ctx, s.log, s.cfg, nil); err != nil {
			return trace.Wrap(err, "joining with token")
		}
	} else {
		if valid {
			// If the identity is valid (not expired), try to renew it.
			newIdentity, err = renewIdentity(ctx, s.log, s.cfg, s.clientBuilder, loadedIdent)
		} else {
			// If the identity has expired, try to join again from scratch.
			newIdentity, err = botIdentityFromToken(ctx, s.log, s.cfg, nil)
		}

		// If there was an identity on-disk from a previous run, but renewing it
		// or re-joining fails, tbot will continue running using the (possibly
		// expired) existing identity.
		//
		// In long-running mode, the Run method will retry the renewal process
		// in case connectivity to the auth server has been restored etc. In the
		// meantime, some services may be able to continue operating with cached
		// data.
		//
		// In one-shot mode, the OneShot method will make a ping RPC to test the
		// connection and exit immediately if the connection is unavailable.
		if err != nil {
			facade := identity.NewFacade(s.cfg.FIPS, s.cfg.Connection.Insecure, loadedIdent)
			client, clientErr := s.clientBuilder.Build(ctx, facade)
			if clientErr != nil {
				return trace.Wrap(clientErr)
			}

			s.mu.Lock()
			s.facade = facade
			s.client = client
			s.mu.Unlock()

			s.log.ErrorContext(ctx, "Failed to renew bot identity. Will attempt to proceed with the old identity, API calls may fail", "error", err)
			return nil
		}
	}

	// We successfully renewed the bot identity!
	s.log.InfoContext(ctx, "Fetched new bot identity", "identity", newIdentity)
	if err := identity.SaveIdentity(ctx, newIdentity, s.cfg.Destination, identity.BotKinds()...); err != nil {
		return trace.Wrap(err)
	}

	facade := identity.NewFacade(s.cfg.FIPS, s.cfg.Connection.Insecure, newIdentity)
	c, err := s.clientBuilder.Build(ctx, facade)
	if err != nil {
		return trace.Wrap(err)
	}
	s.mu.Lock()
	s.client = c
	s.facade = facade
	s.mu.Unlock()

	s.unblockWaiters()
	s.cfg.StatusReporter.Report(readyz.Healthy)

	s.log.InfoContext(ctx, "Identity initialized successfully")
	return nil
}

func (s *Service) Close() error {
	c := s.GetClient()
	if c == nil {
		return nil
	}
	return trace.Wrap(c.Close())
}

func (s *Service) OneShot(ctx context.Context) error {
	if _, err := s.GetClient().Ping(ctx); err != nil {
		return trace.Wrap(err, "testing auth service connection")
	}
	return nil
}

func (s *Service) Run(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "identityService/Run")
	defer span.End()

	// Determine where the bot should write its internal data (renewable cert
	// etc)
	storageDestination := s.cfg.Destination

	// Keep retrying renewal if it failed on startup.
	if !s.IsReady() {
		retry, err := retryutils.NewRetryV2(retryutils.RetryV2Config{
			Driver: retryutils.NewExponentialDriver(1 * time.Second),
			Max:    1 * time.Minute,
			Jitter: retryutils.HalfJitter,
		})
		if err != nil {
			return trace.Wrap(err, "creating retry")
		}

		for {
			retry.Inc()

			s.log.InfoContext(ctx, "Unable to renew bot identity on startup. Waiting to retry", "wait", retry.Duration())

			select {
			case <-retry.After():
			case <-ctx.Done():
				return nil
			}

			if err := s.renew(ctx, storageDestination); err == nil {
				s.unblockWaiters()
				break
			}
		}
	}

	s.log.InfoContext(
		ctx,
		"Beginning bot identity renewal loop",
		"ttl", s.cfg.TTL,
		"interval", s.cfg.RenewalInterval,
	)

	err := internal.RunOnInterval(ctx, internal.RunOnIntervalConfig{
		Service: s.String(),
		Name:    "bot-identity-renewal",
		F: func(ctx context.Context) error {
			return s.renew(ctx, storageDestination)
		},
		Interval:           s.cfg.RenewalInterval,
		RetryLimit:         botIdentityRenewalRetryLimit,
		Log:                s.log,
		ReloadCh:           s.cfg.ReloadCh,
		WaitBeforeFirstRun: true,
		StatusReporter:     s.cfg.StatusReporter,
	})
	return trace.Wrap(err)
}

func (s *Service) renew(
	ctx context.Context,
	botDestination destination.Destination,
) error {
	ctx, span := tracer.Start(ctx, "identityService/renew")
	defer span.End()

	currentIdentity := s.facade.Get()
	// Make sure we can still write to the bot's destination.
	if err := identity.VerifyWrite(ctx, botDestination); err != nil {
		return trace.Wrap(err, "Cannot write to destination %s, aborting.", botDestination)
	}

	newIdentity, err := renewIdentity(ctx, s.log, s.cfg, s.clientBuilder, currentIdentity)
	if err != nil {
		return trace.Wrap(err, "renewing identity")
	}

	s.log.InfoContext(ctx, "Fetched new bot identity", "identity", newIdentity)
	s.facade.Set(newIdentity)

	if err := identity.SaveIdentity(ctx, newIdentity, botDestination, identity.BotKinds()...); err != nil {
		return trace.Wrap(err, "saving new identity")
	}
	s.log.DebugContext(ctx, "Bot identity persisted", "identity", newIdentity)

	return nil
}

func (s *Service) unblockWaiters() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.initialized == nil {
		s.initialized = make(chan struct{})
	}

	s.initializedOnce.Do(func() { close(s.initialized) })
}

// renewIdentity attempts to renew an existing bot identity. "Renewal" in this
// case means one of two things:
//
//  1. If using an explicitly renewable identity (i.e. `token` joining),
//     certificates will be renewed directly via Auth using the formal renewal
//     process.
//
//     If the existing identity is expired, this will fail and cannot be
//     recovered.
//
//  2. For all other join methods, a "lightweight renewal" is performed. The
//     existing client is used to authenticate the request and prove ownership
//     of the existing bot instance ID, but otherwise the delegated joining
//     ceremony is performed as usual.
//
//     If the existing identity appears to be expired (`time.Now()` >
//     `NotAfter`), the existing auth client will be discarded and the bot will
//     try to join without it. This will result in a new bot instance ID.
func renewIdentity(
	ctx context.Context,
	log *slog.Logger,
	cfg Config,
	clientBuilder *client.Builder,
	oldIdentity *identity.Identity,
) (*identity.Identity, error) {
	ctx, span := tracer.Start(ctx, "renewIdentity")
	defer span.End()
	// Explicitly create a new client - this guarantees that requests will be
	// made with the most recent identity and that a connection associated with
	// an old identity will not be used.
	facade := identity.NewFacade(cfg.FIPS, cfg.Connection.Insecure, oldIdentity)
	authClient, err := clientBuilder.Build(ctx, facade)
	if err != nil {
		return nil, trace.Wrap(err, "creating auth client")
	}
	defer authClient.Close()

	if oldIdentity.TLSIdentity.Renewable {
		// When using a renewable join method, we use GenerateUserCerts to
		// request a new certificate using our current identity.
		newIdentity, err := botIdentityFromAuth(
			ctx, log, oldIdentity, authClient, cfg.TTL,
		)
		if err != nil {
			return nil, trace.Wrap(err, "renewing identity using GenerateUserCert")
		}
		return newIdentity, nil
	}

	// Note: This simple expiration check is probably not the best possible
	// solution to determine when to discard an existing identity: the client
	// could have severe clock drift, or there could be non-expiry related
	// reasons that an identity should be thrown out. We may improve this
	// discard logic in the future if we determine we're still creating  excess
	// bot instances.
	now := time.Now()
	if expiry, ok := facade.Expiry(); !ok || now.After(expiry) {
		slog.WarnContext(
			ctx,
			"The bot identity appears to be expired and will not be used to "+
				"authenticate the identity renewal. If it is possible to "+
				"rejoin, a new bot instance will be created. If this occurs "+
				"repeatedly, ensure the local machine's clock is properly "+
				"synchronized, the certificate TTL is adjusted to your "+
				"environment, and that no external issues will prevent "+
				"the bot from renewing its identity on schedule.",
			"now", now,
			"expiry", expiry,
			"ttl", cfg.TTL,
			"renewal_interval", cfg.RenewalInterval,
		)

		newIdentity, err := botIdentityFromToken(ctx, log, cfg, nil)
		if err != nil {
			return nil, trace.Wrap(err, "renewing identity using Register without existing auth client")
		}
		return newIdentity, nil
	}

	newIdentity, err := botIdentityFromToken(ctx, log, cfg, authClient)
	if err != nil {
		return nil, trace.Wrap(err, "renewing identity using Register with existing auth client")
	}
	return newIdentity, nil
}

// botIdentityFromAuth uses an existing identity to request a new from the auth
// server using GenerateUserCerts. This only works for renewable join types.
func botIdentityFromAuth(
	ctx context.Context,
	log *slog.Logger,
	ident *identity.Identity,
	client *apiclient.Client,
	ttl time.Duration,
) (*identity.Identity, error) {
	ctx, span := tracer.Start(ctx, "botIdentityFromAuth")
	defer span.End()
	log.InfoContext(ctx, "Fetching bot identity using existing bot identity")

	if ident == nil || client == nil {
		return nil, trace.BadParameter("renewIdentityWithAuth must be called with non-nil client and identity")
	}

	// Always generate a new key when refreshing the identity. This limits
	// usefulness of compromised keys to the lifetime of their associated cert,
	// and allows for new keys to follow any changes to the signature algorithm
	// suite.
	key, err := cryptosuites.GenerateKey(ctx,
		cryptosuites.GetCurrentSuiteFromAuthPreference(client),
		cryptosuites.HostIdentity)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	privateKeyPEM, err := keys.MarshalPrivateKey(key)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sshPubKey, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sshPub := ssh.MarshalAuthorizedKey(sshPubKey)
	tlsPub, err := keys.MarshalPublicKey(key.Public())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Ask the auth server to generate a new set of certs with a new
	// expiration date.
	certs, err := client.GenerateUserCerts(ctx, proto.UserCertsRequest{
		SSHPublicKey: sshPub,
		TLSPublicKey: tlsPub,
		Username:     ident.X509Cert.Subject.CommonName,
		Expires:      time.Now().Add(ttl),
	})
	if err != nil {
		return nil, trace.Wrap(err, "calling GenerateUserCerts")
	}

	newIdentity, err := identity.ReadIdentityFromStore(&identity.LoadIdentityParams{
		PrivateKeyBytes: privateKeyPEM,
		PublicKeyBytes:  sshPub,
		TokenHashBytes:  ident.TokenHashBytes,
	}, certs)
	if err != nil {
		return nil, trace.Wrap(err, "reading renewed identity")
	}

	return newIdentity, nil
}

// botIdentityFromToken uses a join token to request a bot identity from an auth
// server using auth.Register.
//
// The authClient parameter is optional - if provided - this will be used for
// the request. This saves the overhead of trying to create a new client as
// part of the join process and allows us to preserve the bot instance id.
func botIdentityFromToken(
	ctx context.Context,
	log *slog.Logger,
	cfg Config,
	authClient *apiclient.Client,
) (*identity.Identity, error) {
	_, span := tracer.Start(ctx, "botIdentityFromToken")
	defer span.End()

	log.InfoContext(ctx, "Fetching bot identity using token")

	token, err := cfg.Onboarding.Token()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	cipherSuites := utils.DefaultCipherSuites()
	if cfg.FIPS {
		cipherSuites = defaults.FIPSCipherSuites
	}

	expires := time.Now().Add(cfg.TTL)
	params := join.RegisterParams{
		Token: token,
		ID: state.IdentityID{
			Role: types.RoleBot,
		},
		JoinMethod: cfg.Onboarding.JoinMethod,
		Expires:    &expires,

		// Below options are effectively ignored if AuthClient is not-nil
		Insecure:           cfg.Connection.Insecure,
		CAPins:             cfg.Onboarding.CAPins,
		CAPath:             cfg.Onboarding.CAPath,
		FIPS:               cfg.FIPS,
		GetHostCredentials: libclient.HostCredentials,
		CipherSuites:       cipherSuites,
	}
	if authClient != nil {
		params.AuthClient = authClient
	}

	switch cfg.Connection.AddressKind {
	case connection.AddressKindAuth:
		parsed, err := utils.ParseAddr(cfg.Connection.Address)
		if err != nil {
			return nil, trace.Wrap(err, "failed to parse addr")
		}
		params.AuthServers = []utils.NetAddr{*parsed}
	case connection.AddressKindProxy:
		parsed, err := utils.ParseAddr(cfg.Connection.Address)
		if err != nil {
			return nil, trace.Wrap(err, "failed to parse addr")
		}
		params.ProxyServer = *parsed
	default:
		return nil, trace.BadParameter("unsupported address kind: %v", cfg.Connection.AddressKind)
	}

	// Only set during bound keypair joining, but used both before and after.
	var boundKeypairState *boundkeypair.ClientState

	switch params.JoinMethod {
	case types.JoinMethodAzure:
		params.AzureParams = join.AzureParams{
			ClientID: cfg.Onboarding.Azure.ClientID,
		}
	case types.JoinMethodTerraformCloud:
		params.TerraformCloudAudienceTag = cfg.Onboarding.Terraform.AudienceTag
	case types.JoinMethodEnv0:
		params.Env0AudienceTag = "something"
	case types.JoinMethodGitLab:
		params.GitlabParams = join.GitlabParams{
			EnvVarName: cfg.Onboarding.Gitlab.TokenEnvVarName,
		}
	case types.JoinMethodBoundKeypair:
		joinSecret := cfg.Onboarding.BoundKeypair.RegistrationSecret

		adapter := destination.NewBoundkeypairDestinationAdapter(cfg.Destination)
		boundKeypairState, err = boundkeypair.LoadClientState(ctx, adapter)
		if trace.IsNotFound(err) && joinSecret != "" {
			log.InfoContext(ctx, "No existing client state found, will attempt "+
				"to join with provided registration secret")
			boundKeypairState = boundkeypair.NewEmptyClientState(adapter)
		} else if err != nil {
			log.ErrorContext(ctx, "Could not complete bound keypair joining as "+
				"no local credentials are available and no registration secret "+
				"was provided. To continue, either generate a keypair with "+
				"`tbot keypair create` and register it with Teleport, or "+
				"generate a registration secret on Teleport and provide it with"+
				"the `--registration-secret` flag.")
			return nil, trace.Wrap(err, "loading bound keypair client state")
		}

		params.BoundKeypairParams = boundKeypairState.ToJoinParams(joinSecret)
	}

	result, err := join.Register(ctx, params)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if boundKeypairState != nil {
		if err := boundKeypairState.UpdateFromRegisterResult(result); err != nil {
			return nil, trace.Wrap(err)
		}

		if err := boundKeypairState.Store(ctx); err != nil {
			return nil, trace.Wrap(err)
		}
	}

	privateKeyPEM, err := keys.MarshalPrivateKey(result.PrivateKey)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sshPub, err := ssh.NewPublicKey(result.PrivateKey.Public())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	sha := sha256.Sum256([]byte(params.Token))
	tokenHash := hex.EncodeToString(sha[:])
	ident, err := identity.ReadIdentityFromStore(&identity.LoadIdentityParams{
		PrivateKeyBytes: privateKeyPEM,
		PublicKeyBytes:  ssh.MarshalAuthorizedKey(sshPub),
		TokenHashBytes:  []byte(tokenHash),
	}, result.Certs)
	return ident, trace.Wrap(err)
}
