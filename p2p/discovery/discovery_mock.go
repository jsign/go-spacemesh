package discovery

import (
	"context"
	"github.com/spacemeshos/go-spacemesh/p2p/node"
	"github.com/spacemeshos/go-spacemesh/p2p/p2pcrypto"
)

// MockPeerStore is a mocked discovery
type MockPeerStore struct {
	UpdateFunc      func(n, src node.Node)
	updateCount     int
	SelectPeersFunc func(qty int) []node.Node
	bsres           error
	bsCount         int
	LookupFunc      func(p2pcrypto.PublicKey) (node.Node, error)
	lookupRes       node.Node
	lookupErr       error
}

func (m *MockPeerStore) Remove(key p2pcrypto.PublicKey) {

}

// SetUpdate sets the function to run on an issued update
func (m *MockPeerStore) SetUpdate(f func(n, addr node.Node)) {
	m.UpdateFunc = f
}

// SetLookupResult sets the result ok a lookup operation
func (m *MockPeerStore) SetLookupResult(node node.Node, err error) {
	m.lookupRes = node
	m.lookupErr = err
}

// Update is a discovery update operation it updates the updatecount
func (m *MockPeerStore) Update(n, src node.Node) {
	if m.UpdateFunc != nil {
		m.UpdateFunc(n, src)
	}
	m.updateCount++
}

// UpdateCount returns the number of times update was called
func (m *MockPeerStore) UpdateCount() int {
	return m.updateCount
}

// BootstrapCount returns the number of times bootstrap was called
func (m *MockPeerStore) BootstrapCount() int {
	return m.bsCount
}

// netLookup is a discovery lookup operation
func (m *MockPeerStore) Lookup(pubkey p2pcrypto.PublicKey) (node.Node, error) {
	if m.LookupFunc != nil {
		return m.LookupFunc(pubkey)
	}
	return m.lookupRes, m.lookupErr
}

// SetBootstrap set the bootstrap result
func (m *MockPeerStore) SetBootstrap(err error) {
	m.bsres = err
}

// Bootstrap is a discovery bootstrap operation function it update the bootstrap count
func (m *MockPeerStore) Bootstrap(ctx context.Context) error {
	m.bsCount++
	return m.bsres
}

// SelectPeers mocks selecting peers.
func (m *MockPeerStore) SelectPeers(qty int) []node.Node {
	if m.SelectPeersFunc != nil {
		return m.SelectPeersFunc(qty)
	}
	return []node.Node{}
}

// to satisfy the iface
func (m *MockPeerStore) SetLocalAddresses(tcp, udp string) {

}

// Size returns the size of peers in the discovery
func (m *MockPeerStore) Size() int {
	//todo: set size
	return m.updateCount
}

// mockAddrBook
type mockAddrBook struct {
	addAddressFunc func(n, src NodeInfo)
	addressCount   int

	LookupFunc func(p2pcrypto.PublicKey) (NodeInfo, error)
	lookupRes  NodeInfo
	lookupErr  error

	GetAddressFunc func() *KnownAddress
	GetAddressRes  *KnownAddress

	AddressCacheResult []NodeInfo
}

func (m *mockAddrBook) RemoveAddress(key p2pcrypto.PublicKey) {

}

// SetUpdate sets the function to run on an issued update
func (m *mockAddrBook) SetUpdate(f func(n, addr NodeInfo)) {
	m.addAddressFunc = f
}

// SetLookupResult sets the result ok a lookup operation
func (m *mockAddrBook) SetLookupResult(node NodeInfo, err error) {
	m.lookupRes = node
	m.lookupErr = err
}

// AddAddress mock
func (m *mockAddrBook) AddAddress(n, src NodeInfo) {
	if m.addAddressFunc != nil {
		m.addAddressFunc(n, src)
	}
	m.addressCount++
}

// AddAddresses mock
func (m *mockAddrBook) AddAddresses(n []NodeInfo, src NodeInfo) {
	if m.addAddressFunc != nil {
		for _, addr := range n {
			m.addAddressFunc(addr, src)
			m.addressCount++
		}
	}
}

// AddAddressCount counts AddAddress calls
func (m *mockAddrBook) AddAddressCount() int {
	return m.addressCount
}

// AddressCache mock
func (m *mockAddrBook) AddressCache() []NodeInfo {
	return m.AddressCacheResult
}

// Lookup mock
func (m *mockAddrBook) Lookup(pubkey p2pcrypto.PublicKey) (NodeInfo, error) {
	if m.LookupFunc != nil {
		return m.LookupFunc(pubkey)
	}
	return m.lookupRes, m.lookupErr
}

// GetAddress mock
func (m *mockAddrBook) GetAddress() *KnownAddress {
	if m.GetAddressFunc != nil {
		return m.GetAddressFunc()
	}
	return m.GetAddressRes
}

// NumAddresses mock
func (m *mockAddrBook) NumAddresses() int {
	//todo: mockAddrBook size
	return m.addressCount
}