package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// VideoPricingRule holds durable, group-scoped customer pricing for video operations.
type VideoPricingRule struct {
	ent.Schema
}

func (VideoPricingRule) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "video_pricing_rules"},
	}
}

func (VideoPricingRule) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("group_id"),
		field.String("external_model").MaxLen(128),
		field.Enum("operation").Values("generation", "edit", "extension"),
		field.String("resolution").MaxLen(32).Default("*"),
		field.Enum("audio_mode").Values("any", "with_audio", "without_audio").Default("any"),
		field.Enum("unit").Values("per_request", "per_output_second"),
		field.Float("unit_price").SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}),
		field.Float("upstream_unit_cost").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}),
		field.Bool("enabled").Default(true),
		field.Time("created_at").Immutable().Default(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (VideoPricingRule) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("group", Group.Type).
			Ref("video_pricing_rules").
			Field("group_id").
			Required().
			Unique(),
	}
}

func (VideoPricingRule) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "external_model", "operation", "resolution", "audio_mode").Unique(),
		index.Fields("group_id", "external_model", "operation", "enabled"),
	}
}
