package schema

import (
	"encoding/json"
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// VideoTaskEvent records append-only task lifecycle and operational events.
type VideoTaskEvent struct {
	ent.Schema
}

func (VideoTaskEvent) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "video_task_events"}}
}

func (VideoTaskEvent) Fields() []ent.Field {
	return []ent.Field{
		field.String("request_id").MaxLen(36),
		field.String("event_type").MaxLen(64),
		field.JSON("payload", json.RawMessage{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("created_at").Immutable().Default(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (VideoTaskEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("request_id", "created_at"),
		index.Fields("event_type", "created_at"),
	}
}
