package controller

const (
	// ConditionProvisioned is the primary machine-readable condition that
	// consumers gate on (docs/plan.md §5 status contract). Declared here so
	// schemaclaim, databaseclaim, and databaseprovider can share a single
	// source of truth without cross-importing each other's packages.
	ConditionProvisioned = "Provisioned"

	// ConditionTLSConfiguration tracks whether TLS is disabled, pending, or
	// fully configured for the current resource.
	ConditionTLSConfiguration = "TLSConfiguration"

	ReasonTLSNotEnabled   = "TLSNotEnabled"
	ReasonTLSProvisioning = "TLSProvisioning"
	ReasonTLSConfigured   = "TLSConfigured"

	// Claim Provisioned reasons: shared by claim controllers (when marking)
	// and DatabaseService aggregation (when matching corrective actions).
	ReasonProviderNotFound         = "ProviderNotFound"
	ReasonProviderBound            = "ProviderBound"
	ReasonProviderNotReady         = "ProviderNotReady"
	ReasonDatabaseNotFound         = "DatabaseNotFound"
	ReasonDatabaseCreateNotAllowed = "DatabaseCreateNotAllowed"
	ReasonSchemaCreateNotAllowed   = "SchemaCreateNotAllowed"
	ReasonSchemaReserved           = "SchemaReserved"
	ReasonSchemaNotOwned           = "SchemaNotOwned"
	ReasonSecretNotOwned           = "SecretNotOwned"
	ReasonExtensionNotAvailable    = "ExtensionNotAvailable"
	ReasonExtensionNotSupported    = "ExtensionNotSupported"
	ReasonUserProvisioned          = "UserProvisioned"
	ReasonSchemaProvisioned        = "SchemaProvisioned"
)
