package fixhandler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDecodeDocumentRoots(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantCount int
		wantErr   bool
	}{
		{
			name:      "single document",
			input:     "apiVersion: v1\nkind: Pod\n",
			wantCount: 1,
		},
		{
			name:      "two documents separated by ---",
			input:     "apiVersion: v1\nkind: Pod\n---\napiVersion: v1\nkind: Service\n",
			wantCount: 2,
		},
		{
			name:      "empty string",
			input:     "",
			wantCount: 0,
		},
		{
			name:    "invalid yaml",
			input:   "metadata:\n  name: test\n  bad: [",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes, err := decodeDocumentRoots(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Len(t, nodes, tt.wantCount)
		})
	}
}

func TestRemoveNewLinesAtTheEnd(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "no trailing newlines",
			input:    []string{"line1", "line2"},
			expected: []string{"line1", "line2"},
		},
		{
			name:     "one trailing newline",
			input:    []string{"line1", "line2", "\n"},
			expected: []string{"line1", "line2"},
		},
		{
			name:     "multiple trailing newlines",
			input:    []string{"line1", "line2", "\n", "\n"},
			expected: []string{"line1", "line2"},
		},
		{
			name:     "single element non-newline",
			input:    []string{"line1"},
			expected: []string{"line1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := removeNewLinesAtTheEnd(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFlattenWithDFS(t *testing.T) {
	t.Run("simple scalar node", func(t *testing.T) {
		node := &yaml.Node{
			Kind:  yaml.ScalarNode,
			Value: "hello",
		}
		result := flattenWithDFS(node)
		require.NotNil(t, result)
		assert.Len(t, *result, 1)
		assert.Equal(t, "hello", (*result)[0].node.Value)
		assert.Nil(t, (*result)[0].parent)
	})

	t.Run("mapping node with one key-value pair", func(t *testing.T) {
		node := &yaml.Node{
			Kind: yaml.MappingNode,
			Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "key"},
				{Kind: yaml.ScalarNode, Value: "value"},
			},
		}
		result := flattenWithDFS(node)
		require.NotNil(t, result)
		assert.Len(t, *result, 3)
		assert.Equal(t, yaml.MappingNode, (*result)[0].node.Kind)
		assert.Equal(t, "key", (*result)[1].node.Value)
		assert.Equal(t, "value", (*result)[2].node.Value)
	})

	t.Run("sequence node with items", func(t *testing.T) {
		node := &yaml.Node{
			Kind: yaml.SequenceNode,
			Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "item1"},
				{Kind: yaml.ScalarNode, Value: "item2"},
			},
		}
		result := flattenWithDFS(node)
		require.NotNil(t, result)
		assert.Len(t, *result, 3)
	})

	t.Run("parent references and indexes are set correctly", func(t *testing.T) {
		var root yaml.Node
		require.NoError(t, yaml.Unmarshal([]byte("metadata:\n  name: demo\nspec:\n  replicas: 2\n"), &root))

		result := flattenWithDFS(&root)
		require.NotNil(t, result)
		require.GreaterOrEqual(t, len(*result), 7)
		assert.Same(t, &root, (*result)[0].node)
		assert.Nil(t, (*result)[0].parent)
		assert.Equal(t, 0, (*result)[0].index)
		assert.Same(t, &root, (*result)[1].parent)
		assert.Equal(t, 0, (*result)[1].index)
	})
}

func TestGetFixedNodes(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		expression string
		assert     func(t *testing.T, got []yaml.Node)
		wantError  bool
	}{
		{
			name:       "updates scalar value",
			input:      "spec:\n  replicas: 1\n",
			expression: ".spec.replicas = 3",
			assert: func(t *testing.T, got []yaml.Node) {
				require.Len(t, got, 1)
				var out map[string]map[string]int
				require.NoError(t, got[0].Decode(&out))
				assert.Equal(t, 3, out["spec"]["replicas"])
			},
		},
		{
			name:       "adds mapping key",
			input:      "metadata:\n  name: demo\n",
			expression: ".metadata.namespace = \"default\"",
			assert: func(t *testing.T, got []yaml.Node) {
				require.Len(t, got, 1)
				var out map[string]map[string]string
				require.NoError(t, got[0].Decode(&out))
				assert.Equal(t, "default", out["metadata"]["namespace"])
			},
		},
		{
			name:       "invalid expression",
			input:      "kind: Pod\n",
			expression: ".kind = ",
			wantError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getFixedNodes(context.Background(), tt.input, tt.expression)
			if tt.wantError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			tt.assert(t, got)
		})
	}
}

func TestExtractResourceIdentity(t *testing.T) {
	input := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-dep
  namespace: test-ns
`
	nodes, err := decodeDocumentRoots(input)
	require.NoError(t, err)
	require.Len(t, nodes, 1)

	identity := extractResourceIdentity(&nodes[0])
	assert.Equal(t, "apps/v1", identity.apiVersion)
	assert.Equal(t, "Deployment", identity.kind)
	assert.Equal(t, "my-dep", identity.name)
	assert.Equal(t, "test-ns", identity.namespace)
	assert.False(t, identity.isEmpty())
	assert.Equal(t, "apps/v1|Deployment|test-ns|my-dep", identity.key())
}

func TestIsEmptyOrCommentOnlyDocument(t *testing.T) {
	input := `
---
# Just a comment
---
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
`
	nodes, err := decodeDocumentRoots(input)
	require.NoError(t, err)
	require.Len(t, nodes, 2)

	assert.True(t, isEmptyOrCommentOnlyDocument(&nodes[0]))
	assert.False(t, isEmptyOrCommentOnlyDocument(&nodes[1]))
}

func TestApplyFixToContent_MultiDocumentAlignment(t *testing.T) {
	multiDocYaml := `apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-doc
data:
  key: val
---
# Commented-out document
# apiVersion: v1
# kind: ConfigMap
# metadata:
#   name: commented-cm
---
apiVersion: v1
kind: Pod
metadata:
  name: web-pod
spec:
  containers:
  - name: nginx
    image: nginx:1.14.2
---
# Empty document separator
---
apiVersion: v1
kind: Service
metadata:
  name: web-service
spec:
  ports:
  - port: 80
`

	// Expression fixes the Pod image and adds namespace to Service using yq with(select(...); ...) syntax
	expression := `with(select(.kind == "Pod"); .spec.containers[0].image |= "nginx:1.27") | with(select(.kind == "Service"); .metadata.namespace |= "prod")`

	fixedYaml, err := ApplyFixToContent(context.Background(), multiDocYaml, expression)
	require.NoError(t, err)

	// Verify that comments, empty separator, and documents remain intact
	assert.Contains(t, fixedYaml, "# Commented-out document")
	assert.Contains(t, fixedYaml, "# Empty document separator")
	assert.Contains(t, fixedYaml, "image: nginx:1.27")
	assert.Contains(t, fixedYaml, "namespace: prod")
	assert.Contains(t, fixedYaml, "name: web-service")
}


