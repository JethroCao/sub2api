package schema

import (
	"encoding/json"
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// VideoTask is the authoritative durable ledger for asynchronous video work.
type VideoTask struct {
	ent.Schema
}

type migrationOwnedIndexAnnotation struct {
	IndexName string   `json:"index_name"`
	Fields    []string `json:"fields"`
	Unique    bool     `json:"unique"`
	Predicate string   `json:"predicate"`
}

func (migrationOwnedIndexAnnotation) Name() string { return "MigrationOwnedIndex" }

func (VideoTask) Annotations() []schema.Annotation {
	// The versioned SQL migration owns this table's constraints and indexes.
	// Keep the entity for typed access, but exclude it from Ent auto-migration.
	// A structured annotation records the migration-owned partial index because
	// Ent 0.14 cannot generate a skipped schema that also declares Indexes().
	return []schema.Annotation{
		entsql.Annotation{Table: "video_tasks", Skip: true},
		migrationOwnedIndexAnnotation{
			IndexName: "idx_video_tasks_idempotency",
			Fields:    []string{"user_id", "api_key_id", "idempotency_key_hash"},
			Unique:    true,
			Predicate: "idempotency_key_hash <> ''",
		},
	}
}

func (VideoTask) Fields() []ent.Field {
	return []ent.Field{
		field.String("request_id").Immutable().MaxLen(36),
		field.Int64("user_id"),
		field.Int64("api_key_id"),
		field.Int64("subscription_id").Optional().Nillable(),
		field.Int64("group_id"),
		field.Int64("account_id"),
		field.Enum("platform").Values("video"),
		field.Enum("provider").Values("seedance", "kling"),
		field.Enum("operation").Values("generation", "edit", "extension"),
		field.String("external_model").MaxLen(128),
		field.String("upstream_model").MaxLen(128),
		field.String("idempotency_key_hash").MaxLen(64).Default("").
			Comment("IdempotencyKeyHash stores the caller key digest. Migration 200 enforces a partial unique index with user_id and api_key_id when this value is non-empty."),
		field.String("request_hash").MaxLen(64),
		field.String("provider_submission_token").Optional().Nillable().MaxLen(128),
		field.JSON("request_payload", json.RawMessage{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Enum("status").Values(
			"created", "submitting", "submitted", "queued", "running",
			"succeeded", "failed", "cancelled", "unknown",
		).Default("created"),
		field.String("upstream_task_id").Optional().Nillable().MaxLen(255),
		field.String("upstream_status").Optional().Nillable().MaxLen(64),
		field.String("result_url").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.Time("result_url_expires_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("result_content_type").Optional().Nillable().MaxLen(128),
		field.Float("result_duration_seconds").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "decimal(20,6)"}),
		field.Int("result_width").Optional().Nillable(),
		field.Int("result_height").Optional().Nillable(),
		field.String("pricing_unit").MaxLen(32),
		field.Float("unit_price").SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}),
		field.Float("estimated_units").SchemaType(map[string]string{dialect.Postgres: "decimal(20,6)"}),
		field.Float("estimated_amount").SchemaType(map[string]string{dialect.Postgres: "decimal(20,8)"}),
		field.Float("frozen_amount").SchemaType(map[string]string{dialect.Postgres: "decimal(20,8)"}),
		field.Float("settled_amount").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "decimal(20,8)"}),
		field.String("currency").MaxLen(16).Default("USD"),
		field.String("billing_mode").MaxLen(32),
		field.String("billing_status").MaxLen(32),
		field.String("billing_reference").Optional().Nillable().MaxLen(128),
		field.Int("submission_attempts").Default(0),
		field.Int("poll_attempts").Default(0),
		field.Int("settlement_attempts").Default(0),
		field.Time("next_poll_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("lease_owner").Optional().Nillable().MaxLen(128),
		field.Time("lease_expires_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("last_error_code").Optional().Nillable().MaxLen(128),
		field.String("last_error_message").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.Bool("last_error_retryable").Default(false),
		field.Int64("version").Default(0),
		field.Time("created_at").Immutable().Default(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("submitted_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("started_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("finished_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("settled_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}
