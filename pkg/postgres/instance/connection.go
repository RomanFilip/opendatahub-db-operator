package instance

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

const (
	DefaultAdminUser     = "postgres"
	DefaultAdminDatabase = "postgres"
)

func Host(data Data) string {
	return fmt.Sprintf("%s.%s.svc", data.Service.Name, data.Namespace)
}

func adminDatabase(data Data) string {
	if data.Postgres.DefaultDatabase != "" {
		return data.Postgres.DefaultDatabase
	}
	return DefaultAdminDatabase
}

func AdminSecret(
	data Data,
	password secret.SecretString,
	caData []byte,
) *corev1.Secret {
	// The Kubernetes Secret write is an approved plaintext boundary.
	plaintext := password.Reveal()
	res := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      data.Postgres.AdminSecretName,
			Namespace: data.Namespace,
		},
		Data: map[string][]byte{
			postgres.SecretKeyHost:     []byte(Host(data)),
			postgres.SecretKeyPort:     fmt.Appendf(nil, "%d", postgres.DefaultPort),
			postgres.SecretKeyUser:     []byte(DefaultAdminUser),
			postgres.SecretKeyPassword: []byte(plaintext),
			postgres.SecretKeyDatabase: []byte(adminDatabase(data)),
		},
	}

	if !data.TLS.Enabled {
		return res
	}

	res.Data[postgres.SecretKeySSLMode] = []byte(postgres.SSLModeVerifyFull)
	if len(caData) != 0 {
		res.Data[postgres.SecretKeyCA] = append([]byte(nil), caData...)
	}

	return res
}
