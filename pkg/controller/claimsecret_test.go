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

package controller_test

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	secretvalue "github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

func TestApplyClaimCredentials_ADRContract(t *testing.T) {
	g := NewWithT(t)

	secret := &corev1.Secret{}
	controller.ApplyClaimCredentials(secret, postgres.Config{
		Host:        "db.example.svc",
		Port:        5432,
		SSLMode:     "",
		SSLRootCert: "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----",
	}, "claim_role", secretvalue.FromString("Abc123xyz"), "analytics", "claim-uid")

	g.Expect(secret.Annotations[controller.CredentialStampAnnotation]).NotTo(BeEmpty(),
		"the credential stamp must be recorded for later tamper verification")
	g.Expect(secret.Annotations[postgres.AnnotationConnectionTypeProtocol]).
		To(Equal(postgres.ConnectionTypeProtocolPostgres))
	g.Expect(secret.Type).To(Equal(corev1.SecretTypeOpaque))
	g.Expect(string(secret.Data[postgres.SecretKeyHost])).To(Equal("db.example.svc"))
	g.Expect(string(secret.Data[postgres.SecretKeyPort])).To(Equal("5432"))
	g.Expect(string(secret.Data[postgres.SecretKeyUser])).To(Equal("claim_role"))
	g.Expect(string(secret.Data[postgres.SecretKeyPassword])).To(Equal("Abc123xyz"))
	g.Expect(string(secret.Data[postgres.SecretKeyDatabase])).To(Equal("analytics"))
	g.Expect(string(secret.Data[postgres.SecretKeySSLMode])).To(Equal(postgres.SSLModeDisable))
	g.Expect(string(secret.Data[postgres.SecretKeyCA])).To(ContainSubstring("BEGIN CERTIFICATE"))
	g.Expect(secret.Data).NotTo(HaveKey(postgres.SecretKeySchema))
	g.Expect(secret.Data).NotTo(HaveKey("uri"))
}

func TestApplySchemaClaimCredentials_AddsSchemaKey(t *testing.T) {
	g := NewWithT(t)

	secret := &corev1.Secret{}
	controller.ApplySchemaClaimCredentials(secret, postgres.Config{
		Host: "db.example.svc",
		Port: 5432,
	}, "claim_role", secretvalue.FromString("Abc123xyz"), "analytics", "app_claim", "claim-uid")

	g.Expect(string(secret.Data[postgres.SecretKeySchema])).To(Equal("app_claim"))
	g.Expect(string(secret.Data[postgres.SecretKeyDatabase])).To(Equal("analytics"))
	g.Expect(secret.Annotations[postgres.AnnotationConnectionTypeProtocol]).
		To(Equal(postgres.ConnectionTypeProtocolPostgres))
}
