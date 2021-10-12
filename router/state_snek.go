// Copyright 2021 The Matrix.org Foundation C.I.C.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package router

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/matrix-org/pinecone/types"
	"github.com/matrix-org/pinecone/util"
)

const virtualSnakeMaintainInterval = time.Second
const virtualSnakeNeighExpiryPeriod = time.Hour

type virtualSnakeTable map[virtualSnakeIndex]*virtualSnakeEntry

type virtualSnakeIndex struct {
	PublicKey types.PublicKey
	PathID    types.VirtualSnakePathID
}

type virtualSnakeEntry struct {
	*virtualSnakeIndex
	Origin        types.PublicKey
	Source        *peer
	Destination   *peer
	LastSeen      time.Time
	RootPublicKey types.PublicKey
	RootSequence  types.Varu64
}

func (e *virtualSnakeEntry) valid() bool {
	return time.Since(e.LastSeen) < virtualSnakeNeighExpiryPeriod
}

func (s *state) _maintainSnake() {
	select {
	case <-s.r.context.Done():
		return
	default:
		defer s._maintainSnakeIn(virtualSnakeMaintainInterval)
	}

	rootAnn := s._rootAnnouncement()
	canBootstrap := s._parent != nil && rootAnn.RootPublicKey != s.r.public
	willBootstrap := false

	if asc := s._ascending; asc != nil {
		switch {
		case !asc.valid():
			s._sendTeardownForExistingPath(s.r.local, asc.PublicKey, asc.PathID)
			fallthrough
		case asc.RootPublicKey != rootAnn.RootPublicKey || asc.RootSequence != rootAnn.Sequence:
			willBootstrap = canBootstrap
		}
	} else {
		willBootstrap = canBootstrap
	}

	if desc := s._descending; desc != nil && !desc.valid() {
		s._sendTeardownForExistingPath(s.r.local, desc.PublicKey, desc.PathID)
	}

	// Send bootstrap messages into the network. Ordinarily we
	// would only want to do this when starting up or after a
	// predefined interval, but for now we'll continue to send
	// them on a regular interval until we can derive some better
	// connection state.
	if willBootstrap {
		s._bootstrapNow()
	}
}

func (s *state) _bootstrapNow() {
	if s._parent == nil {
		return
	}
	ann := s._rootAnnouncement()
	if asc := s._ascending; asc != nil && asc.Source.started.Load() {
		if asc.RootPublicKey == ann.RootPublicKey && asc.RootSequence == ann.Sequence {
			return
		}
	}
	b := frameBufferPool.Get().(*[types.MaxFrameSize]byte)
	payload := b[:8+ed25519.PublicKeySize+ann.Sequence.Length()]
	defer frameBufferPool.Put(b)
	bootstrap := types.VirtualSnakeBootstrap{
		RootPublicKey: ann.RootPublicKey,
		RootSequence:  ann.Sequence,
	}
	if _, err := rand.Read(bootstrap.PathID[:]); err != nil {
		return
	}
	if _, err := bootstrap.MarshalBinary(payload[:]); err != nil {
		return
	}
	send := getFrame()
	send.Type = types.TypeVirtualSnakeBootstrap
	send.DestinationKey = s.r.public
	send.Source = s._coords()
	send.Payload = append(send.Payload[:0], payload...)
	if p := s._nextHopsSNEK(s.r.local, send, true); p != nil && p.proto != nil {
		p.proto.push(send)
	}
}

func (s *state) _nextHopsSNEK(from *peer, rx *types.Frame, bootstrap bool) *peer {
	destKey := rx.DestinationKey
	if !bootstrap && s.r.public == destKey {
		return s.r.local
	}
	rootAnn := s._rootAnnouncement()
	bestKey := s.r.public
	var bestPeer *peer
	if !bootstrap {
		bestPeer = s.r.local
	}
	newCandidate := func(key types.PublicKey, p *peer) {
		bestKey, bestPeer = key, p
	}
	newCheckedCandidate := func(candidate types.PublicKey, p *peer) {
		switch {
		case bootstrap && candidate == s.r.public:
			// do nothing
		case !bootstrap && candidate == destKey && bestKey != destKey:
			newCandidate(candidate, p)
		case util.DHTOrdered(destKey, candidate, bestKey):
			newCandidate(candidate, p)
		}
	}

	// Check if we can use the path to the root via our parent as a starting point
	if s._parent != nil && s._parent.started.Load() {
		switch {
		case bootstrap && bestKey == destKey:
			// Bootstraps always start working towards the root so that
			// they go somewhere rather than getting stuck
			fallthrough
		case util.DHTOrdered(bestKey, destKey, rootAnn.RootPublicKey):
			// The destination key is higher than our own key, so
			// start using the path to the root as the first candidate
			newCandidate(rootAnn.RootPublicKey, s._parent)
		}

		// Check our direct ancestors
		// bestKey <= destKey < rootKey
		if ann := s._announcements[s._parent]; ann != nil {
			for _, ancestor := range ann.Signatures {
				newCheckedCandidate(ancestor.PublicKey, s._parent)
			}
		}
	}

	// Check our direct peers ancestors
	for p, ann := range s._announcements {
		if !p.started.Load() {
			continue
		}
		for _, hop := range ann.Signatures {
			newCheckedCandidate(hop.PublicKey, p)
		}
	}

	// Check our direct peers
	for p := range s._announcements {
		if !p.started.Load() {
			continue
		}
		if peerKey := p.public; bestKey == peerKey {
			// We've seen this key already, either as one of our ancestors
			// or as an ancestor of one of our peers, but it turns out we
			// are directly peered with that node, so use the more direct
			// path instead
			newCandidate(peerKey, p)
		}
	}

	// Check our DHT entries
	for _, entry := range s._table {
		if !entry.Source.started.Load() || !entry.valid() || entry.Source == s.r.local {
			continue
		}
		newCheckedCandidate(entry.PublicKey, entry.Source)
	}

	return bestPeer
}

func (s *state) _handleBootstrap(from *peer, rx *types.Frame) error {
	// Unmarshal the bootstrap.
	var bootstrap types.VirtualSnakeBootstrap
	_, err := bootstrap.UnmarshalBinary(rx.Payload)
	if err != nil {
		return fmt.Errorf("bootstrap.UnmarshalBinary: %w", err)
	}
	root := s._rootAnnouncement()
	bootstrapACK := types.VirtualSnakeBootstrapACK{
		PathID:        bootstrap.PathID,
		RootPublicKey: root.RootPublicKey,
		RootSequence:  root.Sequence,
	}
	b := frameBufferPool.Get().(*[types.MaxFrameSize]byte)
	buf := b[:8+ed25519.PublicKeySize+root.Sequence.Length()]
	defer frameBufferPool.Put(b)
	if _, err := bootstrapACK.MarshalBinary(buf[:]); err != nil {
		return fmt.Errorf("bootstrapACK.MarshalBinary: %w", err)
	}
	send := getFrame()
	send.Type = types.TypeVirtualSnakeBootstrapACK
	send.Destination = rx.Source
	send.DestinationKey = rx.DestinationKey
	send.Source = s._coords()
	send.SourceKey = s.r.public
	send.Payload = append(send.Payload[:0], buf...)
	if p := s._nextHopsTree(s.r.local, send); p != nil && p.proto != nil {
		p.proto.push(send)
	}
	return nil
}

func (s *state) _handleBootstrapACK(from *peer, rx *types.Frame) error {
	// Unmarshal the bootstrap ACK.
	var bootstrapACK types.VirtualSnakeBootstrapACK
	_, err := bootstrapACK.UnmarshalBinary(rx.Payload)
	if err != nil {
		return fmt.Errorf("bootstrapACK.UnmarshalBinary: %w", err)
	}
	root := s._rootAnnouncement()
	update := false
	asc := s._ascending
	switch {
	case rx.SourceKey == s.r.public:
		// We received a bootstrap ACK from ourselves. This shouldn't happen,
		// so either another node has forwarded it to us incorrectly, or
		// a routing loop has occurred somewhere. Don't act on the bootstrap
		// in that case.
	case bootstrapACK.RootPublicKey != root.RootPublicKey:
		// Root doesn't match so we won't be able to forward using tree space.
	case bootstrapACK.RootSequence != root.Sequence:
		// Sequence number doesn't match so something is out of date.
	case asc != nil && asc.valid():
		// We already have an ascending entry and it hasn't expired.
		switch {
		case asc.PublicKey == rx.SourceKey && bootstrapACK.PathID != asc.PathID:
			// We've received another bootstrap ACK from our direct ascending node.
			// Just refresh the record and then send a new path setup message to
			// that node.
			update = true
		case util.DHTOrdered(s.r.public, rx.SourceKey, asc.Origin):
			// We know about an ascending node already but it turns out that this
			// new node that we've received a bootstrap from is actually closer to
			// us than the previous node. We'll update our record to use the new
			// node instead and then send a new path setup message to it.
			update = true
		}
	case asc == nil || !asc.valid():
		// We don't have an ascending entry, or we did but it expired.
		if util.LessThan(s.r.public, rx.SourceKey) {
			// We don't know about an ascending node and at the moment we don't know
			// any better candidates, so we'll accept a bootstrap ACK from a node with a
			// key higher than ours (so that it matches descending order).
			update = true
		}
	default:
		// The bootstrap ACK conditions weren't met. This might just be because
		// there's a node out there that hasn't converged to a closer node
		// yet, so we'll just ignore the acknowledgement.
	}
	if !update {
		return nil
	}
	setup := types.VirtualSnakeSetup{ // nolint:gosimple
		PathID:        bootstrapACK.PathID,
		RootPublicKey: root.RootPublicKey,
		RootSequence:  root.Sequence,
	}
	b := frameBufferPool.Get().(*[types.MaxFrameSize]byte)
	buf := b[:8+ed25519.PublicKeySize+root.Sequence.Length()]
	defer frameBufferPool.Put(b)
	if _, err := setup.MarshalBinary(buf[:]); err != nil {
		return fmt.Errorf("setup.MarshalBinary: %w", err)
	}
	send := getFrame()
	send.Type = types.TypeVirtualSnakeSetup
	send.Destination = rx.Source
	send.DestinationKey = rx.SourceKey
	send.SourceKey = s.r.public
	send.Payload = append(send.Payload[:0], buf...)
	nexthop := s.r.state._nextHopsTree(s.r.local, send)
	if nexthop == nil || nexthop.local() || nexthop.proto == nil {
		return nil // fmt.Errorf("no next-hop")
	}
	if !nexthop.proto.push(send) {
		return nil // fmt.Errorf("failed to send setup")
	}
	index := virtualSnakeIndex{
		PublicKey: s.r.public,
		PathID:    bootstrapACK.PathID,
	}
	entry := &virtualSnakeEntry{
		virtualSnakeIndex: &index,
		Origin:            rx.SourceKey,
		Source:            s.r.local,
		Destination:       nexthop,
		LastSeen:          time.Now(),
		RootPublicKey:     bootstrapACK.RootPublicKey,
		RootSequence:      bootstrapACK.RootSequence,
	}
	// Remote side is responsible for clearing up the replaced path, but
	// we do want to make sure we don't have any old paths to other nodes
	// that *aren't* the new ascending node lying around.
	for dhtKey, entry := range s._table {
		if entry.Source == s.r.local && entry.PublicKey != rx.SourceKey {
			s._sendTeardownForExistingPath(s.r.local, dhtKey.PublicKey, dhtKey.PathID)
		}
	}
	// Install the new route into the DHT.
	s._table[index] = entry
	s._ascending = entry
	return nil
}

func (s *state) _handleSetup(from *peer, rx *types.Frame, nexthop *peer) error {
	root := s._rootAnnouncement()
	// Unmarshal the setup.
	var setup types.VirtualSnakeSetup
	if _, err := setup.UnmarshalBinary(rx.Payload); err != nil {
		return fmt.Errorf("setup.UnmarshalBinary: %w", err)
	}
	if setup.RootPublicKey != root.RootPublicKey || setup.RootSequence != root.Sequence {
		s._sendTeardownForRejectedPath(rx.SourceKey, setup.PathID, from)
		return nil // fmt.Errorf("setup root/sequence mismatch")
	}
	index := virtualSnakeIndex{
		PublicKey: rx.SourceKey,
		PathID:    setup.PathID,
	}
	if _, ok := s._table[index]; ok {
		s._sendTeardownForExistingPath(s.r.local, rx.SourceKey, setup.PathID) // first call fixes routing table
		if _, ok := s._table[index]; ok && s.r.debug.Load() {
			panic("should have cleaned up duplicate path in routing table")
		}
		s._sendTeardownForRejectedPath(rx.SourceKey, setup.PathID, from) // second call sends back to origin
		return nil                                                       // fmt.Errorf("setup is a duplicate")
	}
	// If we're at the destination of the setup then update our predecessor
	// with information from the bootstrap.
	if rx.DestinationKey == s.r.public {
		update := false
		desc := s._descending
		switch {
		case setup.RootPublicKey != root.RootPublicKey:
			// Root doesn't match so we won't be able to forward using tree space.
		case setup.RootSequence != root.Sequence:
			// Sequence number doesn't match so something is out of date.
		case !util.LessThan(rx.SourceKey, s.r.public):
			// The bootstrapping key should be less than ours but it isn't.
		case desc != nil && desc.valid():
			// We already have a descending entry and it hasn't expired.
			switch {
			case desc.PublicKey == rx.SourceKey && setup.PathID != desc.PathID:
				// We've received another bootstrap from our direct descending node.
				// Send back an acknowledgement as this is OK.
				update = true
			case util.DHTOrdered(desc.PublicKey, rx.SourceKey, s.r.public):
				// The bootstrapping node is closer to us than our previous descending
				// node was.
				update = true
			}
		case desc == nil || !desc.valid():
			// We don't have a descending entry, or we did but it expired.
			if util.LessThan(rx.SourceKey, s.r.public) {
				// The bootstrapping key is less than ours so we'll acknowledge it.
				update = true
			}
		default:
			// The bootstrap conditions weren't met. This might just be because
			// there's a node out there that hasn't converged to a closer node
			// yet, so we'll just ignore the bootstrap.
		}
		if !update {
			s._sendTeardownForRejectedPath(rx.SourceKey, setup.PathID, from)
			return nil
		}
		if desc != nil {
			// Tear down the previous path, if there was one.
			s._sendTeardownForExistingPath(s.r.local, desc.PublicKey, desc.PathID)
			if s.r.debug.Load() {
				if s._descending != nil {
					panic("should have cleaned up descending node")
				}
				if _, ok := s._table[virtualSnakeIndex{desc.PublicKey, desc.PathID}]; ok {
					panic("should have cleaned up descending entry in routing table")
				}
			}
		}
		entry := &virtualSnakeEntry{
			virtualSnakeIndex: &index,
			Origin:            rx.SourceKey,
			Source:            from,
			Destination:       s.r.local,
			LastSeen:          time.Now(),
			RootPublicKey:     setup.RootPublicKey,
			RootSequence:      setup.RootSequence,
		}
		s._table[index] = entry
		s._descending = entry
		return nil
	}
	// Try to forward the setup onto the next node first. If we
	// can't do that then there's no point in keeping the path.
	if nexthop == nil || nexthop.local() || nexthop.proto == nil || !nexthop.proto.push(rx) {
		s._sendTeardownForRejectedPath(rx.SourceKey, setup.PathID, from)
		return nil // fmt.Errorf("unable to forward setup packet (next-hop %s)", nexthop)
	}
	// Add a new routing table entry as we are intermediate to
	// the path.
	s._table[index] = &virtualSnakeEntry{
		virtualSnakeIndex: &index,
		Origin:            rx.SourceKey,
		LastSeen:          time.Now(),
		RootPublicKey:     setup.RootPublicKey,
		RootSequence:      setup.RootSequence,
		Source:            from,    // node with lower of the two keys
		Destination:       nexthop, // node with higher of the two keys
	}
	return nil
}

func (s *state) _handleTeardown(from *peer, rx *types.Frame) ([]*peer, error) {
	if len(rx.Payload) < 8 {
		return nil, fmt.Errorf("payload too short")
	}
	var teardown types.VirtualSnakeTeardown
	if _, err := teardown.UnmarshalBinary(rx.Payload); err != nil {
		return nil, fmt.Errorf("teardown.UnmarshalBinary: %w", err)
	}
	return s._teardownPath(from, rx.DestinationKey, teardown.PathID), nil
}

func (s *state) _sendTeardownForRejectedPath(pathKey types.PublicKey, pathID types.VirtualSnakePathID, via *peer) {
	if _, ok := s._table[virtualSnakeIndex{pathKey, pathID}]; s.r.debug.Load() && ok {
		panic("rejected path should not be in routing table")
	}
	if via != nil {
		via.proto.push(s._getTeardown(pathKey, pathID))
	}
}

func (s *state) _sendTeardownForExistingPath(from *peer, pathKey types.PublicKey, pathID types.VirtualSnakePathID) {
	frame := s._getTeardown(pathKey, pathID)
	for _, nexthop := range s._teardownPath(from, pathKey, pathID) {
		if nexthop != nil && nexthop.proto != nil {
			nexthop.proto.push(frame)
		}
	}
}

func (s *state) _getTeardown(pathKey types.PublicKey, pathID types.VirtualSnakePathID) *types.Frame {
	var payload [8]byte
	teardown := types.VirtualSnakeTeardown{
		PathID: pathID,
	}
	if _, err := teardown.MarshalBinary(payload[:]); err != nil {
		return nil
	}
	frame := getFrame()
	frame.Type = types.TypeVirtualSnakeTeardown
	frame.DestinationKey = pathKey
	frame.Payload = append(frame.Payload[:0], payload[:]...)
	return frame
}

func (s *state) _teardownPath(from *peer, pathKey types.PublicKey, pathID types.VirtualSnakePathID) []*peer {
	if asc := s._ascending; asc != nil && asc.PublicKey == pathKey && asc.PathID == pathID {
		switch {
		case from.local(): // originated locally
			fallthrough
		case from == asc.Destination: // from network
			s._ascending = nil
			delete(s._table, virtualSnakeIndex{asc.PublicKey, asc.PathID})
			defer s._bootstrapNow()
			return []*peer{asc.Destination}
		}
	}
	if desc := s._descending; desc != nil && desc.PublicKey == pathKey && desc.PathID == pathID {
		switch {
		case from == desc.Source: // from network
			fallthrough
		case from.local(): // originated locally
			s._descending = nil
			delete(s._table, virtualSnakeIndex{desc.PublicKey, desc.PathID})
			return []*peer{desc.Source}
		}
	}
	for k, v := range s._table {
		if k.PublicKey == pathKey && k.PathID == pathID {
			switch {
			case from.local(): // happens when we're tearing down an existing duplicate path
				delete(s._table, k)
				return []*peer{v.Destination, v.Source}
			case from == v.Source: // from network, return the opposite direction
				delete(s._table, k)
				return []*peer{v.Destination}
			case from == v.Destination: // from network, return the opposite direction
				delete(s._table, k)
				return []*peer{v.Source}
			}
		}
	}
	return nil
}
