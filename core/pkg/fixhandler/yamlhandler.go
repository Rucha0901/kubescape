package fixhandler

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kubescape/go-logger"
	"github.com/mikefarah/yq/v4/pkg/yqlib"
	"gopkg.in/yaml.v3"
)

type resourceIdentity struct {
	apiVersion string
	kind       string
	name       string
	namespace  string
}

func (r resourceIdentity) key() string {
	return fmt.Sprintf("%s|%s|%s|%s", r.apiVersion, r.kind, r.namespace, r.name)
}

func (r resourceIdentity) String() string {
	return fmt.Sprintf("apiVersion: %q, kind: %q, name: %q, namespace: %q", r.apiVersion, r.kind, r.name, r.namespace)
}

func (r resourceIdentity) isEmpty() bool {
	return r.apiVersion == "" && r.kind == "" && r.name == ""
}

func (r resourceIdentity) matches(other resourceIdentity) bool {
	if r.isEmpty() || other.isEmpty() {
		return false
	}
	if r.kind != other.kind || r.name != other.name {
		return false
	}
	if r.namespace != "" && other.namespace != "" && r.namespace != other.namespace {
		return false
	}
	if r.apiVersion != "" && other.apiVersion != "" && r.apiVersion != other.apiVersion {
		return false
	}
	return true
}

func extractResourceIdentity(node *yaml.Node) resourceIdentity {
	if node == nil {
		return resourceIdentity{}
	}
	root := node
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return resourceIdentity{}
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return resourceIdentity{}
	}

	var res resourceIdentity
	for i := 0; i+1 < len(root.Content); i += 2 {
		keyNode := root.Content[i]
		valNode := root.Content[i+1]
		if keyNode.Kind != yaml.ScalarNode {
			continue
		}
		switch keyNode.Value {
		case "apiVersion":
			if valNode.Kind == yaml.ScalarNode {
				res.apiVersion = valNode.Value
			}
		case "kind":
			if valNode.Kind == yaml.ScalarNode {
				res.kind = valNode.Value
			}
		case "metadata":
			if valNode.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(valNode.Content); j += 2 {
					mKey := valNode.Content[j]
					mVal := valNode.Content[j+1]
					if mKey.Kind == yaml.ScalarNode && mVal.Kind == yaml.ScalarNode {
						if mKey.Value == "name" {
							res.name = mVal.Value
						} else if mKey.Value == "namespace" {
							res.namespace = mVal.Value
						}
					}
				}
			}
		}
	}
	return res
}

func isEmptyOrCommentOnlyDocument(node *yaml.Node) bool {
	if node == nil {
		return true
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return true
		}
		for _, child := range node.Content {
			if !isEmptyOrCommentOnlyNode(child) {
				return false
			}
		}
		return true
	}
	return isEmptyOrCommentOnlyNode(node)
}

func isEmptyOrCommentOnlyNode(node *yaml.Node) bool {
	if node == nil {
		return true
	}
	if node.Kind == yaml.MappingNode && len(node.Content) == 0 {
		return true
	}
	if node.Kind == yaml.ScalarNode {
		if node.Tag == "!!null" || strings.TrimSpace(node.Value) == "" || node.Value == "null" || node.Value == "{}" {
			return true
		}
	}
	return false
}

// decodeDocumentRoots decodes all YAML documents stored in a given `filepath` and returns a slice of their root nodes
func decodeDocumentRoots(yamlAsString string) ([]yaml.Node, error) {
	fileReader := strings.NewReader(yamlAsString)
	dec := yaml.NewDecoder(fileReader)

	nodes := make([]yaml.Node, 0)
	for {
		var node yaml.Node
		err := dec.Decode(&node)

		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("cannot decode file as YAML")

		}

		nodes = append(nodes, node)
	}

	return nodes, nil
}

func isSameNodeDeep(nodeOne, nodeTwo *yaml.Node) bool {
	if nodeOne == nil || nodeTwo == nil {
		return nodeOne == nodeTwo
	}
	if nodeOne.Kind != nodeTwo.Kind || nodeOne.Value != nodeTwo.Value {
		return false
	}
	if len(nodeOne.Content) != len(nodeTwo.Content) {
		return false
	}
	for i := range nodeOne.Content {
		if !isSameNodeDeep(nodeOne.Content[i], nodeTwo.Content[i]) {
			return false
		}
	}
	return true
}

func hasAnyNodeChanged(origDocs, fixedDocs *list.List) bool {
	if origDocs == nil || fixedDocs == nil || origDocs.Len() != fixedDocs.Len() {
		return true
	}
	origElem := origDocs.Front()
	fixedElem := fixedDocs.Front()
	for origElem != nil && fixedElem != nil {
		origCand, ok1 := origElem.Value.(*yqlib.CandidateNode)
		fixedCand, ok2 := fixedElem.Value.(*yqlib.CandidateNode)
		if ok1 && ok2 && origCand.Node != nil && fixedCand.Node != nil {
			if !isSameNodeDeep(origCand.Node, fixedCand.Node) {
				return true
			}
		}
		origElem = origElem.Next()
		fixedElem = fixedElem.Next()
	}
	return false
}

func getFixedNodes(ctx context.Context, yamlAsString, yamlExpression string) ([]yaml.Node, error) {
	preferences := yqlib.ConfiguredYamlPreferences
	preferences.EvaluateTogether = true
	decoder := yqlib.NewYamlDecoder(preferences)

	var allDocuments = list.New()
	reader := strings.NewReader(yamlAsString)

	fileDocuments, err := readDocuments(ctx, reader, decoder)
	if err != nil {
		return nil, err
	}
	for elem := fileDocuments.Front(); elem != nil; elem = elem.Next() {
		cand, ok := elem.Value.(*yqlib.CandidateNode)
		if ok && cand.Node != nil && !isEmptyOrCommentOnlyDocument(cand.Node) {
			allDocuments.PushBack(cand)
		}
	}

	allAtOnceEvaluator := yqlib.NewAllAtOnceEvaluator()

	fixedCandidateNodes, err := allAtOnceEvaluator.EvaluateCandidateNodes(yamlExpression, allDocuments)

	if err != nil {
		return nil, fmt.Errorf("error fixing YAML, %w", err)
	}

	if strings.HasPrefix(yamlExpression, "select(di==") && !hasAnyNodeChanged(allDocuments, fixedCandidateNodes) {
		if idx := strings.Index(yamlExpression, ")."); idx != -1 {
			fallbackExpr := yamlExpression[idx+1:]
			freshReader := strings.NewReader(yamlAsString)
			freshDecoder := yqlib.NewYamlDecoder(preferences)
			if freshFileDocs, err := readDocuments(ctx, freshReader, freshDecoder); err == nil {
				freshAllDocs := list.New()
				for elem := freshFileDocs.Front(); elem != nil; elem = elem.Next() {
					cand, ok := elem.Value.(*yqlib.CandidateNode)
					if ok && cand.Node != nil && !isEmptyOrCommentOnlyDocument(cand.Node) {
						freshAllDocs.PushBack(cand)
					}
				}
				if fallbackNodes, err := allAtOnceEvaluator.EvaluateCandidateNodes(fallbackExpr, freshAllDocs); err == nil && hasAnyNodeChanged(allDocuments, fallbackNodes) {
					fixedCandidateNodes = fallbackNodes
				}
			}
		}
	}

	fixedNodes := make([]yaml.Node, 0)
	var fixedNode *yaml.Node
	for fixedCandidateNode := fixedCandidateNodes.Front(); fixedCandidateNode != nil; fixedCandidateNode = fixedCandidateNode.Next() {
		fixedNode = fixedCandidateNode.Value.(*yqlib.CandidateNode).Node
		fixedNodes = append(fixedNodes, *fixedNode)
	}

	return fixedNodes, nil
}

func flattenWithDFS(node *yaml.Node) *[]nodeInfo {
	dfsOrder := make([]nodeInfo, 0)
	flattenWithDFSHelper(node, nil, &dfsOrder, 0)
	return &dfsOrder
}

func flattenWithDFSHelper(node *yaml.Node, parent *yaml.Node, dfsOrder *[]nodeInfo, index int) {
	dfsNode := nodeInfo{
		node:   node,
		parent: parent,
		index:  index,
	}
	*dfsOrder = append(*dfsOrder, dfsNode)

	for idx, child := range node.Content {
		flattenWithDFSHelper(child, node, dfsOrder, idx)
	}
}

func getFixInfo(ctx context.Context, originalRootNodes, fixedRootNodes []yaml.Node) (fileFixInfo, error) {
	contentToAdd := make([]contentToAdd, 0)
	linesToRemove := make([]linesToRemove, 0)

	type originalDocEntry struct {
		index    int
		node     *yaml.Node
		identity resourceIdentity
		matched  bool
	}

	origDocs := make([]*originalDocEntry, 0, len(originalRootNodes))
	for i := range originalRootNodes {
		node := &originalRootNodes[i]
		if isEmptyOrCommentOnlyDocument(node) {
			continue
		}
		identity := extractResourceIdentity(node)
		origDocs = append(origDocs, &originalDocEntry{
			index:    i,
			node:     node,
			identity: identity,
			matched:  false,
		})
	}

	for fIdx := range fixedRootNodes {
		fixedNode := &fixedRootNodes[fIdx]
		if isEmptyOrCommentOnlyDocument(fixedNode) {
			continue
		}
		fixedIdentity := extractResourceIdentity(fixedNode)

		var matchedOrig *originalDocEntry

		// 1. Try matching by resource identity (if non-empty)
		if !fixedIdentity.isEmpty() {
			for _, orig := range origDocs {
				if !orig.matched && orig.identity.matches(fixedIdentity) {
					matchedOrig = orig
					break
				}
			}
		}

		// 2. Fallback: match first available unmatched original doc if identity matching didn't yield a match
		if matchedOrig == nil {
			for _, orig := range origDocs {
				if !orig.matched {
					if !fixedIdentity.isEmpty() {
						logger.L().Ctx(ctx).Warning(fmt.Sprintf("Could not determine matching original resource by identity for fixed node (%s); falling back to positional alignment", fixedIdentity.String()))
					}
					matchedOrig = orig
					break
				}
			}
		}

		if matchedOrig == nil {
			logger.L().Ctx(ctx).Warning(fmt.Sprintf("Could not determine matching original resource for fixed node (%s) at fixed index %d", fixedIdentity.String(), fIdx))
			continue
		}

		matchedOrig.matched = true

		originalList := flattenWithDFS(matchedOrig.node)
		fixedList := flattenWithDFS(fixedNode)
		nodeContentToAdd, nodeLinesToRemove, err := getFixInfoHelper(ctx, *originalList, *fixedList)
		if err != nil {
			return fileFixInfo{}, err
		}
		contentToAdd = append(contentToAdd, nodeContentToAdd...)
		linesToRemove = append(linesToRemove, nodeLinesToRemove...)
	}

	return fileFixInfo{
		contentsToAdd: &contentToAdd,
		linesToRemove: &linesToRemove,
	}, nil
}

func getFixInfoHelper(ctx context.Context, originalList, fixedList []nodeInfo) ([]contentToAdd, []linesToRemove, error) {

	// While obtaining fixedYamlNode, comments and empty lines at the top are ignored.
	// This causes a difference in Line numbers across the tree structure. In order to
	// counter this, line numbers are adjusted in fixed list.
	adjustFixedListLines(&originalList, &fixedList)

	contentToAdd := make([]contentToAdd, 0)
	linesToRemove := make([]linesToRemove, 0)

	originalListTracker, fixedListTracker := 0, 0

	fixInfoMetadata := &fixInfoMetadata{
		originalList:        &originalList,
		fixedList:           &fixedList,
		originalListTracker: originalListTracker,
		fixedListTracker:    fixedListTracker,
		contentToAdd:        &contentToAdd,
		linesToRemove:       &linesToRemove,
	}

	for originalListTracker < len(originalList) && fixedListTracker < len(fixedList) {
		matchNodeResult := matchNodes(originalList[originalListTracker].node, fixedList[fixedListTracker].node)

		fixInfoMetadata.originalListTracker = originalListTracker
		fixInfoMetadata.fixedListTracker = fixedListTracker

		var err error
		switch matchNodeResult {
		case sameNodes:
			originalListTracker += 1
			fixedListTracker += 1

		case removedNode:
			originalListTracker, fixedListTracker, err = addLinesToRemove(ctx, fixInfoMetadata)

		case insertedNode:
			originalListTracker, fixedListTracker, err = addLinesToInsert(ctx, fixInfoMetadata)

		case replacedNode:
			originalListTracker, fixedListTracker, err = updateLinesToReplace(ctx, fixInfoMetadata)
		}
		if err != nil {
			return nil, nil, err
		}
	}

	// Some nodes are still not visited if they are removed at the end of the list
	for originalListTracker < len(originalList) {
		fixInfoMetadata.originalListTracker = originalListTracker
		var err error
		originalListTracker, _, err = addLinesToRemove(ctx, fixInfoMetadata)
		if err != nil {
			return nil, nil, err
		}
	}

	// Some nodes are still not visited if they are inserted at the end of the list
	for fixedListTracker < len(fixedList) {
		// Use negative index of last node in original list as a placeholder to determine the last line number later
		fixInfoMetadata.originalListTracker = -(len(originalList) - 1)
		fixInfoMetadata.fixedListTracker = fixedListTracker
		var err error
		_, fixedListTracker, err = addLinesToInsert(ctx, fixInfoMetadata)
		if err != nil {
			return nil, nil, err
		}
	}

	return contentToAdd, linesToRemove, nil

}

// Adds the lines to remove and returns the updated originalListTracker
func addLinesToRemove(ctx context.Context, fixInfoMetadata *fixInfoMetadata) (int, int, error) {
	isOneLine, line := isOneLineSequenceNode(fixInfoMetadata.originalList, fixInfoMetadata.originalListTracker)

	if isOneLine {
		// Remove the entire line and replace it with the sequence node in fixed info. This way,
		// the original formatting is not lost.
		return replaceSingleLineSequence(ctx, fixInfoMetadata, line)
	}

	currentDFSNode := (*fixInfoMetadata.originalList)[fixInfoMetadata.originalListTracker]

	newOriginalListTracker := updateTracker(fixInfoMetadata.originalList, fixInfoMetadata.originalListTracker)
	*fixInfoMetadata.linesToRemove = append(*fixInfoMetadata.linesToRemove, linesToRemove{
		startLine: currentDFSNode.node.Line,
		endLine:   getNodeLine(fixInfoMetadata.originalList, newOriginalListTracker-1), // newOriginalListTracker is the next node
	})

	return newOriginalListTracker, fixInfoMetadata.fixedListTracker, nil
}

// Adds the lines to insert and returns the updated fixedListTracker
func addLinesToInsert(ctx context.Context, fixInfoMetadata *fixInfoMetadata) (int, int, error) {

	isOneLine, line := isOneLineSequenceNode(fixInfoMetadata.fixedList, fixInfoMetadata.fixedListTracker)

	if isOneLine {
		return replaceSingleLineSequence(ctx, fixInfoMetadata, line)
	}

	currentDFSNode := (*fixInfoMetadata.fixedList)[fixInfoMetadata.fixedListTracker]

	lineToInsert := getLineToInsert(fixInfoMetadata)
	contentToInsert, err := getContent(ctx, currentDFSNode.parent, fixInfoMetadata.fixedList, fixInfoMetadata.fixedListTracker)
	if err != nil {
		return 0, 0, err
	}

	newFixedTracker := updateTracker(fixInfoMetadata.fixedList, fixInfoMetadata.fixedListTracker)

	*fixInfoMetadata.contentToAdd = append(*fixInfoMetadata.contentToAdd, contentToAdd{
		line:    lineToInsert,
		content: contentToInsert,
	})

	return fixInfoMetadata.originalListTracker, newFixedTracker, nil
}

// Adds the lines to remove and insert and updates the fixedListTracker and originalListTracker
func updateLinesToReplace(ctx context.Context, fixInfoMetadata *fixInfoMetadata) (int, int, error) {

	isOneLine, line := isOneLineSequenceNode(fixInfoMetadata.fixedList, fixInfoMetadata.fixedListTracker)

	if isOneLine {
		return replaceSingleLineSequence(ctx, fixInfoMetadata, line)
	}

	currentDFSNode := (*fixInfoMetadata.fixedList)[fixInfoMetadata.fixedListTracker]

	// If only the value node is changed, entire "key-value" pair is replaced
	if isValueNodeinMapping(&currentDFSNode) {
		fixInfoMetadata.originalListTracker -= 1
		fixInfoMetadata.fixedListTracker -= 1
	}

	if _, _, err := addLinesToRemove(ctx, fixInfoMetadata); err != nil {
		return 0, 0, err
	}
	updatedOriginalTracker, updatedFixedTracker, err := addLinesToInsert(ctx, fixInfoMetadata)
	if err != nil {
		return 0, 0, err
	}

	return updatedOriginalTracker, updatedFixedTracker, nil
}

func removeNewLinesAtTheEnd(yamlLines []string) []string {
	for idx := 1; idx < len(yamlLines); idx++ {
		if yamlLines[len(yamlLines)-idx] != "\n" {
			yamlLines = yamlLines[:len(yamlLines)-idx+1]
			break
		}
	}
	return yamlLines
}

func getFixedYamlLines(yamlLines []string, fileFixInfo fileFixInfo, newline string) (fixedYamlLines []string) {

	// Determining last line requires original yaml lines slice. The placeholder for last line is replaced with the real last line
	assignLastLine(fileFixInfo.contentsToAdd, fileFixInfo.linesToRemove, &yamlLines)

	removeLines(fileFixInfo.linesToRemove, &yamlLines)

	fixedYamlLines = make([]string, 0)
	lineIdx, lineToAddIdx := 1, 0

	// Ideally, new node is inserted at line before the next node in DFS order. But, when the previous line contains a
	// comment or empty line, we need to insert new nodes before them.
	adjustContentLines(fileFixInfo.contentsToAdd, &yamlLines)

	for lineToAddIdx < len(*fileFixInfo.contentsToAdd) {
		for lineIdx <= (*fileFixInfo.contentsToAdd)[lineToAddIdx].line {
			// Check if the current line is not removed
			if yamlLines[lineIdx-1] != "*" {
				fixedYamlLines = append(fixedYamlLines, yamlLines[lineIdx-1])
			}
			lineIdx += 1
		}

		content := (*fileFixInfo.contentsToAdd)[lineToAddIdx].Content(newline)
		fixedYamlLines = append(fixedYamlLines, content)

		lineToAddIdx += 1
	}

	for lineIdx <= len(yamlLines) {
		if yamlLines[lineIdx-1] != "*" {
			fixedYamlLines = append(fixedYamlLines, yamlLines[lineIdx-1])
		}
		lineIdx += 1
	}

	fixedYamlLines = removeNewLinesAtTheEnd(fixedYamlLines)

	return fixedYamlLines
}
