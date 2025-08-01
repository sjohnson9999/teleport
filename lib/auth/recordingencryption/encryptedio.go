// Teleport
// Copyright (C) 2025 Gravitational, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package recordingencryption

import (
	"context"
	"io"

	"filippo.io/age"
	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/api/types"
)

// SessionRecordingConfigGetter returns the types.SessionRecordingConfig used to determine if
// encryption is enabled and retrieve the encryption keys to use
type SessionRecordingConfigGetter interface {
	GetSessionRecordingConfig(ctx context.Context) (types.SessionRecordingConfig, error)
}

// EncryptedIO wraps a SessionRecordingConfigGetter and a recordingencryption.DecryptionKeyFinder in order
// to provide encryption and decryption wrapping backed by cluster resources
type EncryptedIO struct {
	srcGetter SessionRecordingConfigGetter
	unwrapper KeyUnwrapper
}

// NewEncryptedIO returns an EncryptedIO configured with the given SessionRecordingConfigGetter and
// recordingencryption.DecryptionKeyFinder
func NewEncryptedIO(srcGetter SessionRecordingConfigGetter, unwrapper KeyUnwrapper) (*EncryptedIO, error) {
	switch {
	case srcGetter == nil:
		return nil, trace.BadParameter("SessionRecordingConfigGetter is required for EncryptedIO")
	case unwrapper == nil:
		return nil, trace.BadParameter("DecryptionKeyFinder is required for EncryptedIO")
	}
	return &EncryptedIO{
		srcGetter: srcGetter,
		unwrapper: unwrapper,
	}, nil
}

// WithEncryption wraps the given io.WriteCloser with encryption using the keys present in the
// retrieved types.SessionRecordingConfig
func (e *EncryptedIO) WithEncryption(ctx context.Context, writer io.WriteCloser) (io.WriteCloser, error) {
	src, err := e.srcGetter.GetSessionRecordingConfig(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	encrypter := NewEncryptionWrapper(src)
	w, err := encrypter.WithEncryption(ctx, writer)
	return w, trace.Wrap(err)
}

// WithDecryption wraps the given io.Reader with decryption using the recordingencryption.RecordingIdentity. This
// will dynamically search for an accessible decryption key using the provided recordingencryption.DecryptionKeyFinder
// in order to perform decryption
func (e *EncryptedIO) WithDecryption(ctx context.Context, reader io.Reader) (io.Reader, error) {
	ident := NewRecordingIdentity(ctx, e.unwrapper)
	r, err := age.Decrypt(reader, ident)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return r, nil
}

// EncryptionWrapper provides a wrapper for recording data using the keys present in the given
// types.SessionRecordingConfig
type EncryptionWrapper struct {
	config types.SessionRecordingConfig
}

// NewEncryptionWrapper returns a new EncryptionWrapper backed by the given types.SessionRecordingConfig
func NewEncryptionWrapper(sessionRecordingConfig types.SessionRecordingConfig) *EncryptionWrapper {
	return &EncryptionWrapper{
		config: sessionRecordingConfig,
	}
}

// ErrEncryptionDisabled signals that the [types.SessionRecordingConfig] does not enable encryption.
var ErrEncryptionDisabled = &trace.BadParameterError{Message: "session_recording_config does not enable encryption"}

// WithEncryption wraps the given io.WriteCloser with encryption using the keys present in the
// configured types.SessionRecordingConfig
func (s *EncryptionWrapper) WithEncryption(ctx context.Context, writer io.WriteCloser) (io.WriteCloser, error) {
	if !s.config.GetEncrypted() {
		return nil, trace.Wrap(ErrEncryptionDisabled)
	}

	var recipients []age.Recipient
	for _, key := range s.config.GetEncryptionKeys() {
		recipient, err := ParseRecordingRecipient(key.PublicKey)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		recipients = append(recipients, recipient)
	}

	return &ageWriter{
		w:          writer,
		recipients: recipients,
	}, nil
}

// ageWriter defers initializing the age encrypter to the first write so we can
// prevent age from immediately writing the header
type ageWriter struct {
	w           io.WriteCloser
	recipients  []age.Recipient
	initialized bool
}

// Write data using age encryption, initializing the encrypter if needed
func (a *ageWriter) Write(data []byte) (int, error) {
	if !a.initialized {
		w, err := age.Encrypt(a.w, a.recipients...)
		if err != nil {
			return 0, trace.Wrap(err)
		}
		a.w = w
		a.initialized = true
	}

	return a.w.Write(data)
}

// Close flushes any buffered encrypted data and closes the underlying io.WriteCloser
func (a *ageWriter) Close() error {
	return a.w.Close()
}
