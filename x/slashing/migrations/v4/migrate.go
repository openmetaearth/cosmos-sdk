package v4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/bits-and-blooms/bitset"
	gogotypes "github.com/cosmos/gogoproto/types"

	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/slashing/types"
)

// Migrate replaces the legacy per-position missed-block keys with chunked bitmaps.
func Migrate(ctx sdk.Context, cdc codec.BinaryCodec, store storetypes.KVStore, params types.Params) error {
	start := time.Now()
	validators, byPrefix, err := collectValidators(cdc, store)
	if err != nil || len(validators) == 0 {
		return err
	}

	if err := collectLegacyBitmaps(cdc, store, params.SignedBlocksWindow, validators, byPrefix); err != nil {
		return err
	}

	logger := ctx.Logger()
	if logger != nil {
		var keys, chunks int
		for i := range validators {
			keys += len(validators[i].positions) + len(validators[i].otherKeys)
			chunks += len(validators[i].chunks)
		}
		logger.Info("scanned legacy slashing bitmaps", "validators", len(validators), "legacy_keys", keys, "bitmap_chunks", chunks, "duration", time.Since(start))
	}

	// Neither iterator is open now. Preserve the original order of validators,
	// of their deletions, and of the first write to each chunk.
	start = time.Now()
	for i := range validators {
		if err := validators[i].rewrite(store); err != nil {
			return err
		}
		validators[i] = validatorBitmap{}
	}
	if logger != nil {
		logger.Info("rewrote legacy slashing bitmaps", "duration", time.Since(start))
	}
	return nil
}

type validatorBitmap struct {
	prefix []byte
	// Buffer only the 8-byte suffix of each well-formed key, not the full key.
	positions []uint64
	otherKeys []legacyKey
	chunks    map[int64]*bitset.BitSet
}

type legacyKey struct {
	key                []byte
	precedingPositions int
}

func collectValidators(cdc codec.BinaryCodec, store storetypes.KVStore) (validators []validatorBitmap, byPrefix map[string]int, err error) {
	iter := storetypes.KVStorePrefixIterator(store, ValidatorSigningInfoKeyPrefix)
	defer func() { err = errors.Join(err, iter.Close()) }()

	byPrefix = make(map[string]int)
	for ; iter.Valid(); iter.Next() {
		key := iter.Key()
		// SDK keys are encoded as module prefix + address length + address.
		if len(key) < 3 || len(key) != 2+int(key[1]) {
			return nil, nil, fmt.Errorf("invalid validator signing info key %X", key)
		}
		addr := ValidatorSigningInfoAddress(key)
		if err := sdk.VerifyAddressFormat(addr); err != nil {
			return nil, nil, fmt.Errorf("invalid address in validator signing info key %X: %w", key, err)
		}
		var info types.ValidatorSigningInfo
		if err := cdc.Unmarshal(iter.Value(), &info); err != nil {
			return nil, nil, fmt.Errorf("decode validator signing info at key %X: %w", key, err)
		}
		prefix := validatorMissedBlockBitArrayPrefixKey(addr)
		byPrefix[string(prefix)] = len(validators)
		validators = append(validators, validatorBitmap{prefix: prefix})
	}
	return validators, byPrefix, nil
}

// collectLegacyBitmaps scans existing keys once instead of querying every
// position in every signing window and opening another iterator per validator.
func collectLegacyBitmaps(cdc codec.BinaryCodec, store storetypes.KVStore, window int64, validators []validatorBitmap, byPrefix map[string]int) (err error) {
	iter := storetypes.KVStorePrefixIterator(store, validatorMissedBlockBitArrayKeyPrefix)
	defer func() { err = errors.Join(err, iter.Close()) }()

	for ; iter.Valid(); iter.Next() {
		key := iter.Key()
		if len(key) < 2 {
			continue
		}
		prefixLen := 2 + int(key[1])
		if len(key) < prefixLen {
			continue
		}
		i, ok := byPrefix[string(key[:prefixLen])]
		if !ok {
			// The original migration only touched validators with signing info.
			continue
		}
		v := &validators[i]
		if len(key) != prefixLen+8 {
			// The original prefix deletion also removed unexpected suffixes.
			v.otherKeys = append(v.otherKeys, legacyKey{bytes.Clone(key), len(v.positions)})
			continue
		}
		position := binary.LittleEndian.Uint64(key[prefixLen:])
		v.positions = append(v.positions, position)

		// Delete out-of-window keys without copying their bits, as before.
		index := int64(position)
		if index < 0 || index >= window {
			continue
		}
		var missed gogotypes.BoolValue
		if err := cdc.Unmarshal(iter.Value(), &missed); err != nil {
			return fmt.Errorf("decode legacy missed block at key %X: %w", key, err)
		}
		if !missed.Value {
			continue // Leave zero bits implicit, including wholly empty chunks.
		}

		chunkIndex := index / MissedBlockBitmapChunkSize
		chunk := v.chunks[chunkIndex]
		if chunk == nil {
			if v.chunks == nil {
				v.chunks = make(map[int64]*bitset.BitSet)
			}
			chunk = bitset.New(MissedBlockBitmapChunkSize)
			v.chunks[chunkIndex] = chunk
		}
		chunk.Set(uint(index % MissedBlockBitmapChunkSize))
	}
	return nil
}

func (v validatorBitmap) rewrite(store storetypes.KVStore) error {
	// Old positions and new chunks share the same key space. Delete all old
	// keys for this validator before writing any of the new chunks.
	other := v.otherKeys
	for i, position := range v.positions {
		for len(other) > 0 && other[0].precedingPositions == i {
			store.Delete(other[0].key)
			other = other[1:]
		}
		store.Delete(v.key(position))
	}
	for _, entry := range other {
		store.Delete(entry.key)
	}

	indices := make([]int64, 0, len(v.chunks))
	for index := range v.chunks {
		indices = append(indices, index)
	}
	// The old migration visited positions in numeric order, not KV key order.
	slices.Sort(indices)
	for _, index := range indices {
		bz, err := v.chunks[index].MarshalBinary()
		if err != nil {
			return fmt.Errorf("encode missed-block bitmap chunk %d: %w", index, err)
		}
		store.Set(v.key(uint64(index)), bz)
	}
	return nil
}

func (v validatorBitmap) key(index uint64) []byte {
	key := make([]byte, len(v.prefix)+8)
	copy(key, v.prefix)
	binary.LittleEndian.PutUint64(key[len(v.prefix):], index)
	return key
}

func iterateValidatorMissedBlockBitArray(
	ctx sdk.Context,
	cdc codec.BinaryCodec,
	store storetypes.KVStore,
	addr sdk.ConsAddress,
	params types.Params,
	cb func(index int64, missed bool) (stop bool),
) {
	for i := int64(0); i < params.SignedBlocksWindow; i++ {
		var missed gogotypes.BoolValue
		bz := store.Get(ValidatorMissedBlockBitArrayKey(addr, i))
		if bz == nil {
			continue
		}

		cdc.MustUnmarshal(bz, &missed)
		if cb(i, missed.Value) {
			break
		}
	}
}

func GetValidatorMissedBlocks(
	ctx sdk.Context,
	cdc codec.BinaryCodec,
	store storetypes.KVStore,
	addr sdk.ConsAddress,
	params types.Params,
) []types.MissedBlock {
	var missedBlocks []types.MissedBlock
	iterateValidatorMissedBlockBitArray(ctx, cdc, store, addr, params, func(index int64, missed bool) (stop bool) {
		missedBlocks = append(missedBlocks, types.NewMissedBlock(index, missed))
		return false
	})

	return missedBlocks
}
