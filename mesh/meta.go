// Package mesh is viiwork 2's membership layer: memberlist membership,
// discovery feeders (tailnet, mDNS, seeds) and the member table, plus contract
// C3, the node metadata. It is public so viiwork-gateway can join the mesh as a
// zero-model member.
package mesh

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/janit/viiwork/v2/meshapi"
)

const (
	// MetaVersion is the metadata format this build speaks.
	MetaVersion = 2
	// MetaLimit is memberlist's node metadata limit (memberlist.MetaMaxSize).
	MetaLimit = 512
)

// ErrMetaVersion marks metadata from a node that is not viiwork v2.
var ErrMetaVersion = errors.New("node metadata is not viiwork v2")

// NodeMeta is what a member gossips about itself. Model names are
// deliberately absent: they come from /v1/capacity, which has no size limit.
type NodeMeta struct {
	V    int    `json:"v"`
	API  int    `json:"api"`
	Ver  string `json:"ver"`
	Role string `json:"role"`
}

func (m NodeMeta) validate() error {
	if m.V != MetaVersion {
		return fmt.Errorf("%w: v=%d", ErrMetaVersion, m.V)
	}
	if m.API < 1 || m.API > 65535 {
		return fmt.Errorf("node metadata: api port %d out of range", m.API)
	}
	switch m.Role {
	case meshapi.RoleNode, meshapi.RoleGateway:
	default:
		return fmt.Errorf("node metadata: unknown role %q", m.Role)
	}
	return nil
}

// EncodeMeta validates and serialises metadata for memberlist's NodeMeta.
func EncodeMeta(m NodeMeta) ([]byte, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(b) > MetaLimit {
		return nil, fmt.Errorf("node metadata is %d bytes, memberlist allows %d", len(b), MetaLimit)
	}
	return b, nil
}

// DecodeMeta parses and validates metadata received from a member.
func DecodeMeta(b []byte) (NodeMeta, error) {
	var m NodeMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return NodeMeta{}, fmt.Errorf("node metadata: %w", err)
	}
	if err := m.validate(); err != nil {
		return NodeMeta{}, err
	}
	return m, nil
}
