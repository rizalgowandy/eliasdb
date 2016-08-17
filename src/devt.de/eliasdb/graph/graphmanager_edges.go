/* 
 * EliasDB
 *
 * Copyright 2016 Matthias Ladkau. All rights reserved.
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. 
 */

/*
Edge related API of the graph manager.
*/
package graph

import (
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"sort"
	"strings"

	"devt.de/eliasdb/graph/data"
	"devt.de/eliasdb/graph/util"
	"devt.de/eliasdb/hash"
)

/*
edgeTargetInfo is an internal structure which stores edge information
*/
type edgeTargetInfo struct {
	CascadeToTarget   bool   // Flag if delete operations should be cascaded to the target
	CascadeFromTarget bool   // Flag if delete operations should be cascaded from the target
	TargetNodeKey     string // Key of the target node
	TargetNodeKind    string // Kind of the target ndoe
}

func init() {

	// Make sure we can use the relevant types in a gob operation

	gob.Register(make(map[string]string))
	gob.Register(make(map[string]*edgeTargetInfo))
	gob.Register(&edgeTargetInfo{})
}

/*
Return the edge count for a given edge kind.
*/
func (gm *GraphManager) EdgeCount(kind string) uint64 {

	if val, ok := gm.gs.MainDB()[MAINDB_EDGE_COUNT+kind]; ok {
		return binary.LittleEndian.Uint64([]byte(val))
	}

	return 0
}

/*
FetchNodeEdgeSpecs returns all possible edge specs for a certain node.
*/
func (gm *GraphManager) FetchNodeEdgeSpecs(part string, key string, kind string) ([]string, error) {

	_, tree, err := gm.getNodeStorageHTree(part, kind, false)
	if err != nil || tree == nil {
		return nil, err
	}

	// Take reader lock

	gm.mutex.RLock()
	defer gm.mutex.RUnlock()

	specsNodeKey := PREFIX_NS_SPECS + key
	obj, err := tree.Get([]byte(specsNodeKey))
	if err != nil {
		return nil, &util.GraphError{util.ErrReading, err.Error()}	
	} else if obj == nil {
		return nil, nil
	}

	specsNodeMap := obj.(map[string]string)
	specsNode := make([]string, 0, len(specsNodeMap))

	for spec, _ := range specsNodeMap {
		role1 := gm.nm.Decode16(spec[:2])
		relKind := gm.nm.Decode16(spec[2:4])
		role2 := gm.nm.Decode16(spec[4:6])
		end2Kind := gm.nm.Decode16(spec[6:])

		specsNode = append(specsNode,
			role1+":"+relKind+":"+role2+":"+end2Kind)
	}

	// Ensure the output is deterministic

	sort.StringSlice(specsNode).Sort()

	return specsNode, nil
}

/*
TraverseMulti traverses from a given node to other nodes following a given
partial edge spec. Since the edge spec can be partial it is possible to
traverse multiple edge kinds. A spec with the value ":::" would follow
all relationships. The last parameter allData specifies if all data
should be retrieved for the connected nodes and edges. If set to false only
the minimal set of attributes will be populated.
*/
func (gm *GraphManager) TraverseMulti(part string, key string, kind string,
	spec string, allData bool) ([]data.Node, []data.Edge, error) {

	sspec := strings.Split(spec, ":")
	if len(sspec) != 4 {
		return nil, nil, &util.GraphError{util.ErrInvalidData, "Invalid spec: " + spec}
	} else if IsFullSpec(spec) {
		return gm.Traverse(part, key, kind, spec, allData)
	}

	// Get all specs for the given node

	specs, err := gm.FetchNodeEdgeSpecs(part, key, kind)
	if err != nil || specs == nil {
		return nil, nil, err
	}

	matchSpec := func(spec string) bool {
		mspec := strings.Split(spec, ":")

		// Check spec components

		if (sspec[0] != "" && mspec[0] != sspec[0]) ||
			(sspec[1] != "" && mspec[1] != sspec[1]) ||
			(sspec[2] != "" && mspec[2] != sspec[2]) ||
			(sspec[3] != "" && mspec[3] != sspec[3]) {

			return false
		}

		return true
	}

	// Match specs and collect the results

	nodes := make([]data.Node, 0)
	edges := make([]data.Edge, 0)

	for _, rspec := range specs {
		if spec == ":::" || matchSpec(rspec) {

			sn, se, err := gm.Traverse(part, key, kind, rspec, allData)
			if err != nil {
				return nil, nil, err
			}

			nodes = append(nodes, sn...)
			edges = append(edges, se...)
		}
	}

	return nodes, edges, nil
}

/*
Traverse traverses from a given node to other nodes following a given edge spec.
The last parameter allData specifies if all data should be retrieved for
the connected nodes and edges. If set to false only the minimal set of
attributes will be populated.
*/
func (gm *GraphManager) Traverse(part string, key string, kind string,
	spec string, allData bool) ([]data.Node, []data.Edge, error) {

	_, tree, err := gm.getNodeStorageHTree(part, kind, false)
	if err != nil || tree == nil {
		return nil, nil, err
	}

	// Take reader lock

	gm.mutex.RLock()
	defer gm.mutex.RUnlock()

	sspec := strings.Split(spec, ":")
	if len(sspec) != 4 {
		return nil, nil, &util.GraphError{util.ErrInvalidData, "Invalid spec: " + spec}
	} else if !IsFullSpec(spec) {
		return nil, nil, &util.GraphError{util.ErrInvalidData, "Invalid spec: " + spec +
			" - spec needs to be fully specified for direct traversal"}
	}

	encspec := gm.nm.Encode16(sspec[0], false) + gm.nm.Encode16(sspec[1], false) +
		gm.nm.Encode16(sspec[2], false) + gm.nm.Encode16(sspec[3], false)

	edgeInfoKey := PREFIX_NS_EDGE + key + encspec

	// Lookup the target map containing edgeTargetInfo objects

	obj, err := tree.Get([]byte(edgeInfoKey))
	if err != nil || obj == nil {
		return nil, nil, err
	}

	targetMap := obj.(map[string]*edgeTargetInfo)

	nodes := make([]data.Node, 0, len(targetMap))
	edges := make([]data.Edge, 0, len(targetMap))

	if !allData {

		// Populate nodes and edges with the minimal set of attributes
		// no further lookups required

		for k, v := range targetMap {

			edge := data.NewGraphEdge()

			edge.SetAttr(data.NODE_KEY, k)
			edge.SetAttr(data.NODE_KIND, sspec[1])

			edge.SetAttr(data.EDGE_END1_KEY, key)
			edge.SetAttr(data.EDGE_END1_KIND, kind)
			edge.SetAttr(data.EDGE_END1_ROLE, sspec[0])
			edge.SetAttr(data.EDGE_END1_CASCADING, v.CascadeToTarget)

			edge.SetAttr(data.EDGE_END2_KEY, v.TargetNodeKey)
			edge.SetAttr(data.EDGE_END2_KIND, v.TargetNodeKind)
			edge.SetAttr(data.EDGE_END2_ROLE, sspec[2])
			edge.SetAttr(data.EDGE_END2_CASCADING, v.CascadeFromTarget)

			edges = append(edges, edge)

			node := data.NewGraphNode()

			node.SetAttr(data.NODE_KEY, v.TargetNodeKey)
			node.SetAttr(data.NODE_KIND, v.TargetNodeKind)

			nodes = append(nodes, node)
		}

	} else {

		// Get the HTrees which stores the edges

		edgeht, err := gm.getEdgeStorageHTree(part, sspec[1], false)
		if err != nil || edgeht == nil {
			return nil, nil, err
		}

		for k, v := range targetMap {

			// Read the edge from the datastore

			edgenode, err := gm.readNode(k, sspec[1], nil, edgeht, edgeht)
			if err != nil || edgenode == nil {
				return nil, nil, err
			}
			edge := data.NewGraphEdgeFromNode(edgenode)

			// Exchange ends if necessary

			if edge.End2Key() == key && edge.End2Kind() == kind {
				swap := func(attr1 string, attr2 string) {
					tmp := edge.Attr(attr1)
					edge.SetAttr(attr1, edge.Attr(attr2))
					edge.SetAttr(attr2, tmp)
				}

				swap(data.EDGE_END1_KEY, data.EDGE_END2_KEY)
				swap(data.EDGE_END1_KIND, data.EDGE_END2_KIND)
				swap(data.EDGE_END1_ROLE, data.EDGE_END2_ROLE)
				swap(data.EDGE_END1_CASCADING, data.EDGE_END2_CASCADING)
			}

			edges = append(edges, edge)

			// Get the HTrees which stores the node

			attht, valht, err := gm.getNodeStorageHTree(part, v.TargetNodeKind, false)
			if err != nil || attht == nil || valht == nil {
				return nil, nil, err
			}

			node, err := gm.readNode(v.TargetNodeKey, v.TargetNodeKind, nil, attht, valht)
			if err != nil {
				return nil, nil, err
			}

			nodes = append(nodes, node)
		}
	}

	return nodes, edges, nil
}

/*
FetchEdge fetches a single edge from a partition of the graph.
*/
func (gm *GraphManager) FetchEdge(part string, key string, kind string) (data.Node, error) {
	return gm.FetchEdgePart(part, key, kind, nil)
}

/*
FetchEdgePart fetches part of a single edge from a partition of the graph.
*/
func (gm *GraphManager) FetchEdgePart(part string, key string, kind string,
	attrs []string) (data.Edge, error) {

	// Get the HTrees which stores the edge

	edgeht, err := gm.getEdgeStorageHTree(part, kind, true)
	if err != nil || edgeht == nil {
		return nil, err
	}

	// Take reader lock

	gm.mutex.RLock()
	defer gm.mutex.RUnlock()

	// Read the edge from the datastore

	node, err := gm.readNode(key, kind, attrs, edgeht, edgeht)

	return data.NewGraphEdgeFromNode(node), err
}

/*
StoreEdge stores a single edge in a partition of the graph. This function will
overwrites any existing edge.
*/
func (gm *GraphManager) StoreEdge(part string, edge data.Edge) error {

	// Check if the edge can be stored

	if err := gm.checkEdge(edge); err != nil {
		return err
	}

	// Get the HTrees which stores the edges and the edge index

	iht, err := gm.getEdgeIndexHTree(part, edge.Kind(), true)
	if err != nil {
		return err
	}

	edgeht, err := gm.getEdgeStorageHTree(part, edge.Kind(), true)
	if err != nil {
		return err
	}

	// Get the HTrees which stores the edge endpoints and make sure the endpoints
	// do exist

	end1nodeht, end1ht, err := gm.getNodeStorageHTree(part, edge.End1Kind(), false)

	if err != nil {
		return err
	} else if end1ht == nil {
		return &util.GraphError{util.ErrInvalidData,
			"Can't store edge to non-existend node kind: " + edge.End1Kind()}
	} else if end1, err := end1nodeht.Get([]byte(PREFIX_NS_ATTRS + edge.End1Key())); err != nil || end1 == nil {
		return &util.GraphError{util.ErrInvalidData,
			fmt.Sprintf("Can't find edge endpoint: %s (%s)",
				edge.End1Key(), edge.End1Kind())}
	}

	end2nodeht, end2ht, err := gm.getNodeStorageHTree(part, edge.End2Kind(), false)

	if err != nil {
		return err
	} else if end2ht == nil {
		return &util.GraphError{util.ErrInvalidData,
			"Can't store edge to non-existend node kind: " + edge.End2Kind()}
	} else if end2, err := end2nodeht.Get([]byte(PREFIX_NS_ATTRS + edge.End2Key())); err != nil || end2 == nil {
		return &util.GraphError{util.ErrInvalidData,
			fmt.Sprintf("Can't find edge endpoint: %s (%s)",
				edge.End2Key(), edge.End2Kind())}
	}

	// Take writer lock

	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	// Write edge to the datastore

	oldedge, err := gm.writeEdge(edge, edgeht, end1ht, end2ht)
	if err != nil {
		return err
	}

	// Increase edge count if the edge was inserted and write the changes
	// to the index.

	if oldedge == nil {

		// Increase edge count

		current_count := gm.EdgeCount(edge.Kind())
		if err := gm.writeEdgeCount(edge.Kind(), current_count+1, true); err != nil {
			return err
		}

		// Write edge data to the index

		if iht != nil {

			if err := util.NewIndexManager(iht).Index(edge.Key(), edge.IndexMap()); err != nil {

				// The edge was written at this point and the model is
				// consistent only the index is missing entries

				return err
			}
		}

	} else if iht != nil {

		err := util.NewIndexManager(iht).Reindex(edge.Key(), edge.IndexMap(),
			oldedge.IndexMap())

		if err != nil {

			// The edge was written at this point and the model is
			// consistent only the index is missing entries

			return err
		}
	}

	// Execute rules

	trans := NewGraphTrans(gm)
	trans.subtrans = true

	var event int
	if oldedge == nil {
		event = EVENT_EDGE_CREATED
	} else {
		event = EVENT_EDGE_UPDATED
	}

	if err := gm.gr.graphEvent(trans, event, part, edge, oldedge); err != nil {
		return err
	} else if err := trans.Commit(); err != nil {
		return err
	}

	// Flush changes - errors only reported on the actual node storage flush

	gm.gs.FlushMain()

	gm.flushEdgeIndex(part, edge.Kind())

	gm.flushNodeStorage(part, edge.End1Kind())

	gm.flushNodeStorage(part, edge.End2Kind())

	return gm.flushEdgeStorage(part, edge.Kind())
}

/*
writeEdge writes a given edge to the datastore. It is assumed that the caller
holds the writer lock before calling the functions and that, after the function
returns, the changes are flushed to the storage. The caller has also to ensure
that the endpoints of the edge do exist. Returns the old edge if an
update occurred.
*/
func (gm *GraphManager) writeEdge(edge data.Edge, edgeTree *hash.HTree,
	end1Tree *hash.HTree, end2Tree *hash.HTree) (data.Edge, error) {

	// Create lookup keys

	spec1 := gm.nm.Encode16(edge.End1Role(), true) + gm.nm.Encode16(edge.Kind(), true) +
		gm.nm.Encode16(edge.End2Role(), true) + gm.nm.Encode16(edge.End2Kind(), true)

	spec2 := gm.nm.Encode16(edge.End2Role(), true) + gm.nm.Encode16(edge.Kind(), true) +
		gm.nm.Encode16(edge.End1Role(), true) + gm.nm.Encode16(edge.End1Kind(), true)

	specsNode1Key := PREFIX_NS_SPECS + edge.End1Key()
	edgeInfo1Key := PREFIX_NS_EDGE + edge.End1Key() + spec1

	specsNode2Key := PREFIX_NS_SPECS + edge.End2Key()
	edgeInfo2Key := PREFIX_NS_EDGE + edge.End2Key() + spec2

	// Function to insert a new spec into a specs map

	updateSpecMap := func(key string, spec string, tree *hash.HTree) error {
		var specsNode map[string]string

		obj, err := tree.Get([]byte(key))

		if err != nil {
			return err
		} else if obj == nil {
			specsNode = make(map[string]string)
		} else {
			specsNode = obj.(map[string]string)
		}

		specsNode[spec] = ""

		if _, err = tree.Put([]byte(key), specsNode); err != nil {
			return err
		}

		return nil
	}

	// Function to update the edgeTargetInfo entry

	updateTargetInfo := func(key string, endkey string, endkind string,
		cascadeToTarget bool, cascadeFromTarget bool, tree *hash.HTree) error {

		var targetMap map[string]*edgeTargetInfo

		obj, err := tree.Get([]byte(key))

		if err != nil {
			return err
		} else if obj == nil {
			targetMap = make(map[string]*edgeTargetInfo)
		} else {
			targetMap = obj.(map[string]*edgeTargetInfo)
		}

		// Update the target info

		targetMap[edge.Key()] = &edgeTargetInfo{cascadeToTarget,
			cascadeFromTarget, endkey, endkind}

		if _, err = tree.Put([]byte(key), targetMap); err != nil {
			return err
		}

		return nil
	}

	// Write node data for edge - if the data is incorrect we write the old
	// data back later. It is assumed that most of the time the data is correct
	// so we can avoid an extra read lookup

	var oldedge data.Edge

	if oldedgenode, err := gm.writeNode(edge, false, edgeTree, edgeTree, edgeAttributeFilter); err != nil {
		return nil, err
	} else if oldedgenode != nil {
		oldedge = data.NewGraphEdgeFromNode(oldedgenode)

		// Do a sanity check that the endpoints were not updated.

		if !data.NodeCompare(oldedge, edge, []string{data.EDGE_END1_KEY,
			data.EDGE_END1_KIND, data.EDGE_END1_ROLE, data.EDGE_END2_KEY,
			data.EDGE_END2_KIND, data.EDGE_END2_ROLE}) {

			// If the check fails then write back the old data and return
			// no error checking when writing back

			gm.writeNode(oldedge, false, edgeTree, edgeTree, edgeAttributeFilter)

			return nil, &util.GraphError{util.ErrInvalidData, "Cannot update " +
				"endpoints or spec of existing edge: " + edge.Key()}
		}

		return oldedge, nil
	}

	// Create / update specs map on the nodes

	if err := updateSpecMap(specsNode1Key, spec1, end1Tree); err != nil {
		return nil, err
	}
	if err := updateSpecMap(specsNode2Key, spec2, end2Tree); err != nil {
		return nil, err
	}

	// Create / update the edgeInfo entries

	if err := updateTargetInfo(edgeInfo1Key, edge.End2Key(), edge.End2Kind(),
		edge.End1IsCascading(), edge.End2IsCascading(), end1Tree); err != nil {
		return nil, err
	}

	if err := updateTargetInfo(edgeInfo2Key, edge.End1Key(), edge.End1Kind(),
		edge.End2IsCascading(), edge.End1IsCascading(), end2Tree); err != nil {
		return nil, err
	}

	return nil, nil
}

/*
RemoveNode removes a single node from a partition of the graph.
*/
func (gm *GraphManager) RemoveEdge(part string, key string, kind string) (data.Edge, error) {

	// Get the HTrees which stores the edges and the edge index

	iht, err := gm.getEdgeIndexHTree(part, kind, true)
	if err != nil {
		return nil, err
	}

	edgeht, err := gm.getEdgeStorageHTree(part, kind, true)
	if err != nil {
		return nil, err
	}

	// Take writer lock

	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	// Delete the node from the datastore

	node, err := gm.deleteNode(key, kind, edgeht, edgeht)
	edge := data.NewGraphEdgeFromNode(node)
	if err != nil {
		return edge, err
	}

	if node != nil {

		// Get the HTrees which stores the edge endpoints

		_, end1ht, err := gm.getNodeStorageHTree(part, edge.End1Kind(), false)
		if err != nil {
			return edge, err
		}

		_, end2ht, err := gm.getNodeStorageHTree(part, edge.End2Kind(), false)
		if err != nil {
			return edge, err
		}

		// Delete edge info from node storage

		if err := gm.deleteEdge(edge, end1ht, end2ht); err != nil {
			return edge, err
		}

		if iht != nil {
			err := util.NewIndexManager(iht).Deindex(key, edge.IndexMap())
			if err != nil {
				return edge, err
			}
		}

		// Decrease edge count

		current_count := gm.EdgeCount(edge.Kind())
		if err := gm.writeEdgeCount(edge.Kind(), current_count-1, true); err != nil {
			return edge, err
		}

		// Execute rules

		trans := NewGraphTrans(gm)
		trans.subtrans = true

		if err := gm.gr.graphEvent(trans, EVENT_EDGE_DELETED, part, edge); err != nil {
			return edge, err
		} else if err := trans.Commit(); err != nil {
			return edge, err
		}

		// Flush changes - errors only reported on the actual node storage flush

		gm.gs.FlushMain()

		gm.flushEdgeIndex(part, edge.Kind())

		gm.flushNodeStorage(part, edge.End1Kind())

		gm.flushNodeStorage(part, edge.End2Kind())

		return edge, gm.flushEdgeStorage(part, edge.Kind())
	}

	return nil, nil
}

/*
Delete edge information from a given node storage
*/
func (gm *GraphManager) deleteEdge(edge data.Edge, end1Tree *hash.HTree, end2Tree *hash.HTree) error {

	// Create lookup keys

	spec1 := gm.nm.Encode16(edge.End1Role(), true) + gm.nm.Encode16(edge.Kind(), true) +
		gm.nm.Encode16(edge.End2Role(), true) + gm.nm.Encode16(edge.End2Kind(), true)

	spec2 := gm.nm.Encode16(edge.End2Role(), true) + gm.nm.Encode16(edge.Kind(), true) +
		gm.nm.Encode16(edge.End1Role(), true) + gm.nm.Encode16(edge.End1Kind(), true)

	specsNode1Key := PREFIX_NS_SPECS + edge.End1Key()
	edgeInfo1Key := PREFIX_NS_EDGE + edge.End1Key() + spec1

	specsNode2Key := PREFIX_NS_SPECS + edge.End2Key()
	edgeInfo2Key := PREFIX_NS_EDGE + edge.End2Key() + spec2

	// Function to delete a spec from a specs map

	updateSpecMap := func(key string, spec string, tree *hash.HTree) error {
		var specsNode map[string]string

		obj, err := tree.Get([]byte(key))

		if err != nil {
			return &util.GraphError{util.ErrReading, err.Error()}
		} else if obj == nil {
			return &util.GraphError{util.ErrInvalidData,
				fmt.Sprintf("Expected spec entry is missing: %v", key)}
		} else {
			specsNode = obj.(map[string]string)
		}

		delete(specsNode, spec)

		if len(specsNode) == 0 {

			if _, err = tree.Remove([]byte(key)); err != nil {
				return err
			}

		} else if _, err = tree.Put([]byte(key), specsNode); err != nil {
			return err
		}

		return nil
	}

	// Function to delete the edgeTargetInfo entry

	updateTargetInfo := func(key string, tree *hash.HTree) (bool, error) {

		var targetMap map[string]*edgeTargetInfo

		obj, err := tree.Get([]byte(key))

		if err != nil {
			return false, &util.GraphError{util.ErrReading, err.Error()}
		} else if obj == nil {
			return false, &util.GraphError{util.ErrInvalidData,
				fmt.Sprintf("Expected edgeTargetInfo entry is missing: %v", key)}
		} else {
			targetMap = obj.(map[string]*edgeTargetInfo)
		}

		delete(targetMap, edge.Key())

		if len(targetMap) == 0 {

			if _, err = tree.Remove([]byte(key)); err != nil {
				return false, err
			}

			return true, nil

		} else if _, err = tree.Put([]byte(key), targetMap); err != nil {
			return false, err
		}

		return false, nil
	}

	// Remove the edgeInfo entries

	end1TargetInfoRemoved, err := updateTargetInfo(edgeInfo1Key, end1Tree)
	if err != nil {
		return err
	}

	end2TargetInfoRemoved, err := updateTargetInfo(edgeInfo2Key, end2Tree)
	if err != nil {
		return err
	}

	// Remove specs map on the nodes if the target info structure was removed

	if end1TargetInfoRemoved {
		if err := updateSpecMap(specsNode1Key, spec1, end1Tree); err != nil {
			return err
		}
	}

	if end2TargetInfoRemoved {
		if err := updateSpecMap(specsNode2Key, spec2, end2Tree); err != nil {
			return err
		}
	}

	return nil
}

/*
Default filter function to filter out system edge attributes.
*/
func edgeAttributeFilter(attr string) bool {
	return attr == data.NODE_KEY || attr == data.NODE_KIND
}
