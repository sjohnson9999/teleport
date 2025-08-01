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

package env0

type envGetter func(key string) string

// IDTokenSource allows an Env0 token to be fetched whilst
// within an Env0 execution.
type IDTokenSource struct {
	audienceTag string

	getEnv envGetter
}

// GetIDToken fetches an Env0 JWT from the local node's environment
func (its *IDTokenSource) GetIDToken() (string, error) {
	name := "ENV0_OIDC_TOKEN"

	tok := its.getEnv(name)

	return tok, nil
}

// NewIDTokenSource creates a new Env0 ID token source with the given audience
// tag.
func NewIDTokenSource(audienceTag string, getEnv envGetter) *IDTokenSource {
	return &IDTokenSource{
		audienceTag,
		getEnv,
	}
}
