/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/resources/gvk"
	secretvalue "github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

// CredentialStampAnnotation carries a hash of (claim UID, role, password)
// recorded whenever the operator writes a credentials Secret. On later
// reconciles it proves whether the stored password is the one this claim
// provisioned, without contacting PostgreSQL: a match means the content is
// ours (heal or preserve it), a mismatch on an owned Secret means the
// password was tampered with (rotate), and its absence on an unowned Secret
// means the Secret is foreign (reject). The stamp is forgeable by anyone who
// can write the Secret, but that actor can already read the credentials; the
// stamp defends integrity against accidental drift, not in-namespace malice.
// It verifies Secret-internal consistency only — it never consults
// PostgreSQL, so it cannot detect the role's password changing out-of-band.
const CredentialStampAnnotation = "db.infrastructure.opendatahub.io/credential-stamp"

func credentialStamp(claimUID types.UID, role, password string) string {
	digest := sha256.New()
	digest.Write([]byte(claimUID))
	digest.Write([]byte{0})
	digest.Write([]byte(role))
	digest.Write([]byte{0})
	digest.Write([]byte(password))
	return hex.EncodeToString(digest.Sum(nil))
}

// HasCredentialStamp reports whether the Secret carries a stamp at all. A
// stamped Secret that fails CredentialStampMatches was tampered with; an
// unstamped one predates the stamp (migration preserves it once ownership
// is established).
func HasCredentialStamp(secret *corev1.Secret) bool {
	return secret != nil && secret.Annotations[CredentialStampAnnotation] != ""
}

// CredentialStampMatches reports whether the Secret exists, carries a stamp,
// and its stored password still hashes to that stamp for this claim and role.
func CredentialStampMatches(secret *corev1.Secret, claimUID types.UID, role string) bool {
	if secret == nil || secret.ResourceVersion == "" {
		return false
	}
	stamp, ok := secret.Annotations[CredentialStampAnnotation]
	if !ok || stamp == "" {
		return false
	}
	return stamp == credentialStamp(claimUID, role,
		secretvalue.FromBytes(secret.Data[postgres.SecretKeyPassword]).Reveal())
}

// ApplyClaimCredentials populates an Opaque postgres Connection Secret with the
// ADR credential contract: discrete keys, connection-type annotation, and
// always-present sslmode (defaulting to disable when unset).
func ApplyClaimCredentials(
	secret *corev1.Secret,
	cfg postgres.Config,
	role string,
	password secretvalue.SecretString,
	database string,
	claimUID types.UID,
) {
	// The Kubernetes Secret write is an approved plaintext boundary.
	plaintext := password.Reveal()
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[postgres.AnnotationConnectionTypeProtocol] = postgres.ConnectionTypeProtocolPostgres
	secret.Annotations[CredentialStampAnnotation] = credentialStamp(claimUID, role, plaintext)
	secret.SetGroupVersionKind(gvk.Secret)
	secret.Type = corev1.SecretTypeOpaque

	sslmode := cfg.SSLMode
	if sslmode == "" {
		sslmode = postgres.SSLModeDisable
	}

	secret.Data = map[string][]byte{
		postgres.SecretKeyHost:     []byte(cfg.Host),
		postgres.SecretKeyPort:     []byte(strconv.Itoa(cfg.Port)),
		postgres.SecretKeyUser:     []byte(role),
		postgres.SecretKeyPassword: []byte(plaintext),
		postgres.SecretKeyDatabase: []byte(database),
		postgres.SecretKeySSLMode:  []byte(sslmode),
	}
	if cfg.SSLRootCert != "" {
		secret.Data[postgres.SecretKeyCA] = []byte(cfg.SSLRootCert)
	}
}

func ApplySchemaClaimCredentials(
	secret *corev1.Secret,
	cfg postgres.Config,
	role string,
	password secretvalue.SecretString,
	database string,
	schema string,
	claimUID types.UID,
) {
	ApplyClaimCredentials(secret, cfg, role, password, database, claimUID)
	if schema != "" {
		secret.Data[postgres.SecretKeySchema] = []byte(schema)
	}
}

// ProviderReachable reports whether the provider has Reachable=True.
func ProviderReachable(conditions []fwapi.Condition) bool {
	for _, c := range conditions {
		if c.Type == "Reachable" && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}
