package v4_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/bits-and-blooms/bitset"
	gogotypes "github.com/cosmos/gogoproto/types"
	"github.com/stretchr/testify/require"

	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/testutil"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"
	"github.com/cosmos/cosmos-sdk/x/slashing"
	v4 "github.com/cosmos/cosmos-sdk/x/slashing/migrations/v4"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
)

var consAddr = sdk.ConsAddress(sdk.AccAddress([]byte("addr1_______________")))

func TestMigrate(t *testing.T) {
	cdc := moduletestutil.MakeTestEncodingConfig(slashing.AppModuleBasic{}).Codec
	storeKey := storetypes.NewKVStoreKey(slashingtypes.ModuleName)
	tKey := storetypes.NewTransientStoreKey("transient_test")
	ctx := testutil.DefaultContext(storeKey, tKey)
	store := ctx.KVStore(storeKey)
	params := slashingtypes.Params{SignedBlocksWindow: 100}

	// store old signing info and bitmap entries
	bz := cdc.MustMarshal(&slashingtypes.ValidatorSigningInfo{Address: consAddr.String()})
	store.Set(v4.ValidatorSigningInfoKey(consAddr), bz)

	for i := int64(0); i < params.SignedBlocksWindow; i++ {
		// all even blocks are missed
		missed := &gogotypes.BoolValue{Value: i%2 == 0}
		bz := cdc.MustMarshal(missed)
		store.Set(v4.ValidatorMissedBlockBitArrayKey(consAddr, i), bz)
	}

	err := v4.Migrate(ctx, cdc, store, params)
	require.NoError(t, err)

	for i := int64(0); i < params.SignedBlocksWindow; i++ {
		chunkIndex := i / v4.MissedBlockBitmapChunkSize
		chunk := store.Get(v4.ValidatorMissedBlockBitmapKey(consAddr, chunkIndex))
		require.NotNil(t, chunk)

		bs := bitset.New(uint(v4.MissedBlockBitmapChunkSize))
		require.NoError(t, bs.UnmarshalBinary(chunk))

		// ensure all even blocks are missed
		bitIndex := uint(i % v4.MissedBlockBitmapChunkSize)
		require.Equal(t, i%2 == 0, bs.Test(bitIndex))
		require.Equal(t, i%2 == 1, !bs.Test(bitIndex))
	}

	// ensure there's only one chunk for a window of size 100
	chunk := store.Get(v4.ValidatorMissedBlockBitmapKey(consAddr, 1))
	require.Nil(t, chunk)
}

func TestMigrateMatchesLegacyCommit(t *testing.T) {
	cdc := moduletestutil.MakeTestEncodingConfig(slashing.AppModuleBasic{}).Codec
	storeKey := storetypes.NewKVStoreKey(slashingtypes.ModuleName)
	tKey := storetypes.NewTransientStoreKey("transient_test")
	params := slashingtypes.Params{SignedBlocksWindow: 3*v4.MissedBlockBitmapChunkSize + 1}
	const boundary = v4.MissedBlockBitmapChunkSize
	otherAddr := sdk.ConsAddress([]byte("addr2_______________"))
	inactive := sdk.ConsAddress([]byte("inactive____________"))
	orphan := sdk.ConsAddress([]byte("orphan______________"))
	orphanKey := v4.ValidatorMissedBlockBitArrayKey(orphan, 0)
	unexpectedKey := append(v4.ValidatorMissedBlockBitArrayKey(otherAddr, 256), 0)

	seed := func(ctx sdk.Context) {
		s := ctx.KVStore(storeKey)
		for _, addr := range []sdk.ConsAddress{consAddr, otherAddr, inactive} {
			info := slashingtypes.ValidatorSigningInfo{
				Address: addr.String(), StartHeight: 42, IndexOffset: 5000,
				MissedBlocksCounter: 4, JailedUntil: time.Unix(100, 0).UTC(), Tombstoned: true,
			}
			s.Set(v4.ValidatorSigningInfoKey(addr), cdc.MustMarshal(&info))
		}
		missed := cdc.MustMarshal(&gogotypes.BoolValue{Value: true})
		signed := cdc.MustMarshal(&gogotypes.BoolValue{Value: false})
		// Positions 0 and 1 collide with output chunk keys. Use different
		// bit sets per validator, with holes, written false values and an empty chunk.
		for _, pos := range []int64{0, 256, boundary - 1, boundary + 3, 3 * boundary} {
			s.Set(v4.ValidatorMissedBlockBitArrayKey(consAddr, pos), missed)
		}
		for _, pos := range []int64{1, 2 * boundary} {
			s.Set(v4.ValidatorMissedBlockBitArrayKey(consAddr, pos), signed)
		}
		// Out-of-window and malformed entries are deleted but not decoded.
		for _, pos := range []int64{-1, params.SignedBlocksWindow, params.SignedBlocksWindow + 200} {
			s.Set(v4.ValidatorMissedBlockBitArrayKey(consAddr, pos), []byte{0xff})
		}
		s.Set(v4.ValidatorMissedBlockBitArrayKey(otherAddr, boundary), missed)
		s.Set(unexpectedKey, []byte{0xff})
		s.Set(orphanKey, missed)
	}

	legacy, optimized := testutil.DefaultContextWithDB(t, storeKey, tKey), testutil.DefaultContextWithDB(t, storeKey, tKey)
	seed(legacy.Ctx)
	seed(optimized.Ctx)
	require.Equal(t, legacy.CMS.Commit(), optimized.CMS.Commit())

	legacyCtx, writeLegacy := legacy.Ctx.CacheContext()
	legacyStore := legacyCtx.KVStore(storeKey)
	migrateLegacyForTest(t, legacyCtx, legacyStore, params, []sdk.ConsAddress{consAddr, otherAddr, inactive})
	writeLegacy()

	optimizedCtx, writeOptimized := optimized.Ctx.CacheContext()
	optimizedStore := &migrationOps{KVStore: optimizedCtx.KVStore(storeKey)}
	require.NoError(t, v4.Migrate(optimizedCtx, cdc, optimizedStore, params))
	require.Zero(t, optimizedStore.gets)
	require.Equal(t, 2, optimizedStore.iterators)
	require.Equal(t, 4, optimizedStore.sets) // Three nonempty chunks for consAddr, one for otherAddr.
	writeOptimized()
	require.Equal(t, legacy.CMS.Commit(), optimized.CMS.Commit())
	committed := optimized.Ctx.KVStore(storeKey)
	require.NotNil(t, committed.Get(orphanKey))
	require.Nil(t, committed.Get(unexpectedKey))
	require.Nil(t, committed.Get(v4.ValidatorMissedBlockBitmapKey(consAddr, 2)))
}

// migrateLegacyForTest uses the pre-optimization point reads and per-bit
// writes. It deliberately does not share the optimized migration's scan or
// chunk-building code. The fixture supplies the signing-info addresses in key order.
func migrateLegacyForTest(t testing.TB, ctx sdk.Context, store storetypes.KVStore, params slashingtypes.Params, addrs []sdk.ConsAddress) {
	t.Helper()
	cdc := moduletestutil.MakeTestEncodingConfig(slashing.AppModuleBasic{}).Codec
	missed := make([][]slashingtypes.MissedBlock, len(addrs))
	for i, addr := range addrs {
		missed[i] = v4.GetValidatorMissedBlocks(ctx, cdc, store, addr, params)
	}
	for i, addr := range addrs {
		key := v4.ValidatorMissedBlockBitArrayKey(addr, 0)
		iter := storetypes.KVStorePrefixIterator(store, key[:len(key)-8])
		for ; iter.Valid(); iter.Next() {
			store.Delete(iter.Key())
		}
		require.NoError(t, iter.Close())
		for _, block := range missed[i] {
			if !block.Missed {
				continue
			}
			key := v4.ValidatorMissedBlockBitmapKey(addr, block.Index/v4.MissedBlockBitmapChunkSize)
			bits := bitset.New(v4.MissedBlockBitmapChunkSize)
			if bz := store.Get(key); bz != nil {
				require.NoError(t, bits.UnmarshalBinary(bz))
			}
			bits.Set(uint(block.Index % v4.MissedBlockBitmapChunkSize))
			bz, err := bits.MarshalBinary()
			require.NoError(t, err)
			store.Set(key, bz)
		}
	}
}

type migrationOps struct {
	storetypes.KVStore
	gets, iterators, sets int
}

func (s *migrationOps) Get(key []byte) []byte {
	s.gets++
	return s.KVStore.Get(key)
}

func (s *migrationOps) Iterator(start, end []byte) storetypes.Iterator {
	s.iterators++
	return s.KVStore.Iterator(start, end)
}

func (s *migrationOps) Set(key, value []byte) {
	s.sets++
	s.KVStore.Set(key, value)
}

func BenchmarkMigrate(b *testing.B) {
	cdc := moduletestutil.MakeTestEncodingConfig(slashing.AppModuleBasic{}).Codec
	storeKey := storetypes.NewKVStoreKey(slashingtypes.ModuleName)
	tKey := storetypes.NewTransientStoreKey("transient_test")
	params := slashingtypes.Params{SignedBlocksWindow: 10_000}
	for _, n := range []int64{32, params.SignedBlocksWindow} {
		b.Run(fmt.Sprintf("positions=%d", n), func(b *testing.B) {
			fixture := testutil.DefaultContextWithDB(b, storeKey, tKey)
			store := fixture.Ctx.KVStore(storeKey)
			missed := cdc.MustMarshal(&gogotypes.BoolValue{Value: true})
			signed := cdc.MustMarshal(&gogotypes.BoolValue{Value: false})
			addrs := make([]sdk.ConsAddress, 40)
			for i := range addrs {
				addr := sdk.ConsAddress(fmt.Sprintf("%020d", i))
				addrs[i] = addr
				store.Set(v4.ValidatorSigningInfoKey(addr), cdc.MustMarshal(&slashingtypes.ValidatorSigningInfo{Address: addr.String()}))
				for j := int64(0); j < n; j++ {
					value := signed
					if j%13 == 0 {
						value = missed
					}
					store.Set(v4.ValidatorMissedBlockBitArrayKey(addr, j), value)
				}
			}
			fixture.CMS.Commit()
			b.Run("legacy", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					ctx, _ := fixture.Ctx.CacheContext()
					migrateLegacyForTest(b, ctx, ctx.KVStore(storeKey), params, addrs)
				}
			})
			b.Run("optimized", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					ctx, _ := fixture.Ctx.CacheContext()
					require.NoError(b, v4.Migrate(ctx, cdc, ctx.KVStore(storeKey), params))
				}
			})
		})
	}
}
