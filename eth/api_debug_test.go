// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"bytes"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
)

var dumper = spew.ConfigState{Indent: "    "}

type Account struct {
	key  *ecdsa.PrivateKey
	addr common.Address
}

func newAccounts(n int) (accounts []Account) {
	for i := 0; i < n; i++ {
		key, _ := crypto.GenerateKey()
		addr := crypto.PubkeyToAddress(key.PublicKey)
		accounts = append(accounts, Account{key: key, addr: addr})
	}
	slices.SortFunc(accounts, func(a, b Account) int { return a.addr.Cmp(b.addr) })
	return accounts
}

// newTestBlockChain creates a new test blockchain. OBS: After test is done, teardown must be
// invoked in order to release associated resources.
func newTestBlockChain(t *testing.T, n int, gspec *core.Genesis, generator func(i int, b *core.BlockGen)) *core.BlockChain {
	engine := ethash.NewFaker()
	// Generate blocks for testing
	_, blocks, _ := core.GenerateChainWithGenesis(gspec, engine, n, generator)

	// Import the canonical chain
	options := &core.BlockChainConfig{
		TrieCleanLimit: 256,
		TrieDirtyLimit: 256,
		TrieTimeLimit:  5 * time.Minute,
		SnapshotLimit:  0,
		Preimages:      true,
		ArchiveMode:    true, // Archive mode
	}
	chain, err := core.NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	if n, err := chain.InsertChain(blocks); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}
	return chain
}

func accountRangeTest(t *testing.T, trie *state.Trie, statedb *state.StateDB, start common.Hash, requestedNum int, expectedNum int) state.Dump {
	result := statedb.RawDump(&state.DumpConfig{
		SkipCode:          true,
		SkipStorage:       true,
		OnlyWithAddresses: false,
		Start:             start.Bytes(),
		Max:               uint64(requestedNum),
	})

	if len(result.Accounts) != expectedNum {
		t.Fatalf("expected %d results, got %d", expectedNum, len(result.Accounts))
	}
	for addr, acc := range result.Accounts {
		if strings.HasSuffix(addr, "pre") || acc.Address == nil {
			t.Fatalf("account without prestate (address) returned: %v", addr)
		}
		if !statedb.Exist(*acc.Address) {
			t.Fatalf("account not found in state %s", acc.Address.Hex())
		}
	}
	return result
}

func TestAccountRange(t *testing.T) {
	t.Parallel()

	var (
		mdb     = rawdb.NewMemoryDatabase()
		statedb = state.NewDatabase(triedb.NewDatabase(mdb, &triedb.Config{Preimages: true}), nil)
		sdb, _  = state.New(types.EmptyRootHash, statedb)
		addrs   = [AccountRangeMaxResults * 2]common.Address{}
		m       = map[common.Address]bool{}
	)

	for i := range addrs {
		hash := common.HexToHash(fmt.Sprintf("%x", i))
		addr := common.BytesToAddress(crypto.Keccak256Hash(hash.Bytes()).Bytes())
		addrs[i] = addr
		sdb.SetBalance(addrs[i], uint256.NewInt(1), tracing.BalanceChangeUnspecified)
		if _, ok := m[addr]; ok {
			t.Fatalf("bad")
		} else {
			m[addr] = true
		}
	}
	root, _ := sdb.Commit(0, true, false)
	sdb, _ = state.New(root, statedb)

	trie, err := statedb.OpenTrie(root)
	if err != nil {
		t.Fatal(err)
	}
	accountRangeTest(t, &trie, sdb, common.Hash{}, AccountRangeMaxResults/2, AccountRangeMaxResults/2)
	// test pagination
	firstResult := accountRangeTest(t, &trie, sdb, common.Hash{}, AccountRangeMaxResults, AccountRangeMaxResults)
	secondResult := accountRangeTest(t, &trie, sdb, common.BytesToHash(firstResult.Next), AccountRangeMaxResults, AccountRangeMaxResults)

	hList := make([]common.Hash, 0)
	for addr1, acc := range firstResult.Accounts {
		// If address is non-available, then it makes no sense to compare
		// them as they might be two different accounts.
		if acc.Address == nil {
			continue
		}
		if _, duplicate := secondResult.Accounts[addr1]; duplicate {
			t.Fatalf("pagination test failed:  results should not overlap")
		}
		hList = append(hList, crypto.Keccak256Hash(acc.Address.Bytes()))
	}
	// Test to see if it's possible to recover from the middle of the previous
	// set and get an even split between the first and second sets.
	slices.SortFunc(hList, common.Hash.Cmp)
	middleH := hList[AccountRangeMaxResults/2]
	middleResult := accountRangeTest(t, &trie, sdb, middleH, AccountRangeMaxResults, AccountRangeMaxResults)
	missing, infirst, insecond := 0, 0, 0
	for h := range middleResult.Accounts {
		if _, ok := firstResult.Accounts[h]; ok {
			infirst++
		} else if _, ok := secondResult.Accounts[h]; ok {
			insecond++
		} else {
			missing++
		}
	}
	if missing != 0 {
		t.Fatalf("%d hashes in the 'middle' set were neither in the first not the second set", missing)
	}
	if infirst != AccountRangeMaxResults/2 {
		t.Fatalf("Imbalance in the number of first-test results: %d != %d", infirst, AccountRangeMaxResults/2)
	}
	if insecond != AccountRangeMaxResults/2 {
		t.Fatalf("Imbalance in the number of second-test results: %d != %d", insecond, AccountRangeMaxResults/2)
	}
}

func TestEmptyAccountRange(t *testing.T) {
	t.Parallel()

	var (
		statedb = state.NewDatabaseForTesting()
		st, _   = state.New(types.EmptyRootHash, statedb)
	)
	// Commit(although nothing to flush) and re-init the statedb
	st.Commit(0, true, false)
	st, _ = state.New(types.EmptyRootHash, statedb)

	results := st.RawDump(&state.DumpConfig{
		SkipCode:          true,
		SkipStorage:       true,
		OnlyWithAddresses: true,
		Max:               uint64(AccountRangeMaxResults),
	})
	if bytes.Equal(results.Next, (common.Hash{}).Bytes()) {
		t.Fatalf("Empty results should not return a second page")
	}
	if len(results.Accounts) != 0 {
		t.Fatalf("Empty state should not return addresses: %v", results.Accounts)
	}
}

func TestStorageRangeAt(t *testing.T) {
	t.Parallel()

	// Create a state where account 0x010000... has a few storage entries.
	var (
		mdb    = rawdb.NewMemoryDatabase()
		tdb    = triedb.NewDatabase(mdb, &triedb.Config{Preimages: true})
		db     = state.NewDatabase(tdb, nil)
		sdb, _ = state.New(types.EmptyRootHash, db)
		addr   = common.Address{0x01}
		keys   = []common.Hash{ // hashes of Keys of storage
			common.HexToHash("340dd630ad21bf010b4e676dbfa9ba9a02175262d1fa356232cfde6cb5b47ef2"),
			common.HexToHash("426fcb404ab2d5d8e61a3d918108006bbb0a9be65e92235bb10eefbdb6dcd053"),
			common.HexToHash("48078cfed56339ea54962e72c37c7f588fc4f8e5bc173827ba75cb10a63a96a5"),
			common.HexToHash("5723d2c3a83af9b735e3b7f21531e5623d183a9095a56604ead41f3582fdfb75"),
		}
		storage = storageMap{
			keys[0]: {Key: &common.Hash{0x02}, Value: common.Hash{0x01}},
			keys[1]: {Key: &common.Hash{0x04}, Value: common.Hash{0x02}},
			keys[2]: {Key: &common.Hash{0x01}, Value: common.Hash{0x03}},
			keys[3]: {Key: &common.Hash{0x03}, Value: common.Hash{0x04}},
		}
	)
	for _, entry := range storage {
		sdb.SetState(addr, *entry.Key, entry.Value)
	}
	root, _ := sdb.Commit(0, false, false)
	sdb, _ = state.New(root, db)

	// Check a few combinations of limit and start/end.
	tests := []struct {
		start []byte
		limit int
		want  StorageRangeResult
	}{
		{
			start: []byte{}, limit: 0,
			want: StorageRangeResult{storageMap{}, &keys[0]},
		},
		{
			start: []byte{}, limit: 100,
			want: StorageRangeResult{storage, nil},
		},
		{
			start: []byte{}, limit: 2,
			want: StorageRangeResult{storageMap{keys[0]: storage[keys[0]], keys[1]: storage[keys[1]]}, &keys[2]},
		},
		{
			start: []byte{0x00}, limit: 4,
			want: StorageRangeResult{storage, nil},
		},
		{
			start: []byte{0x40}, limit: 2,
			want: StorageRangeResult{storageMap{keys[1]: storage[keys[1]], keys[2]: storage[keys[2]]}, &keys[3]},
		},
	}
	for _, test := range tests {
		result, err := storageRangeAt(sdb, root, addr, test.start, test.limit)
		if err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(result, test.want) {
			t.Fatalf("wrong result for range %#x.., limit %d:\ngot %s\nwant %s",
				test.start, test.limit, dumper.Sdump(result), dumper.Sdump(&test.want))
		}
	}
}

func TestGetModifiedAccounts(t *testing.T) {
	t.Parallel()

	// Initialize test accounts
	accounts := newAccounts(4)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
			accounts[2].addr: {Balance: big.NewInt(params.Ether)},
			accounts[3].addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	genBlocks := 1
	signer := types.HomesteadSigner{}
	blockChain := newTestBlockChain(t, genBlocks, genesis, func(_ int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		for _, account := range accounts[:3] {
			tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
				Nonce:    0,
				To:       &accounts[3].addr,
				Value:    big.NewInt(1000),
				Gas:      params.TxGas,
				GasPrice: b.BaseFee(),
				Data:     nil}),
				signer, account.key)
			b.AddTx(tx)
		}
	})
	defer blockChain.Stop()

	// Create a debug API instance.
	api := NewDebugAPI(&Ethereum{blockchain: blockChain})

	// Test GetModifiedAccountsByNumber
	t.Run("GetModifiedAccountsByNumber", func(t *testing.T) {
		addrs, err := api.GetModifiedAccountsByNumber(uint64(genBlocks), nil)
		assert.NoError(t, err)
		assert.Len(t, addrs, len(accounts)+1) // +1 for the coinbase
		for _, account := range accounts {
			if !slices.Contains(addrs, account.addr) {
				t.Fatalf("account %s not found in modified accounts", account.addr.Hex())
			}
		}
	})

	// Test GetModifiedAccountsByHash
	t.Run("GetModifiedAccountsByHash", func(t *testing.T) {
		header := blockChain.GetHeaderByNumber(uint64(genBlocks))
		addrs, err := api.GetModifiedAccountsByHash(header.Hash(), nil)
		assert.NoError(t, err)
		assert.Len(t, addrs, len(accounts)+1) // +1 for the coinbase
		for _, account := range accounts {
			if !slices.Contains(addrs, account.addr) {
				t.Fatalf("account %s not found in modified accounts", account.addr.Hex())
			}
		}
	})
}
