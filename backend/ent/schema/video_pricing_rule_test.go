package schema

import (
	"testing"

	"entgo.io/ent/dialect/entsql"
	entschema "entgo.io/ent/schema"
	"github.com/stretchr/testify/require"
)

func TestVideoPricingRuleGroupForeignKeyCascadesOnDelete(t *testing.T) {
	var annotations []entschema.Annotation
	for _, groupEdge := range (Group{}).Edges() {
		if groupEdge.Descriptor().Name == "video_pricing_rules" {
			annotations = groupEdge.Descriptor().Annotations
			break
		}
	}
	require.Len(t, annotations, 1)
	annotation, ok := annotations[0].(*entsql.Annotation)
	require.True(t, ok)
	require.Equal(t, entsql.Cascade, annotation.OnDelete)
}
