package types

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// Geometry is the verified physical block geometry of an APFS container.
//
// Every conversion from a physical block number to a byte offset must use the
// geometry of the container the block belongs to. A Geometry is derived from
// a container superblock, validated before the mount becomes visible, and is
// afterwards immutable: values are safe to share between goroutines, but a
// Geometry must never be shared between different containers (not even two
// images opened from the same path), because their declared block sizes may
// differ.
type Geometry struct {
	blockSize uint64
}

// DefaultGeometry is the 4 KiB geometry. It is used to bootstrap reading of
// the container superblock (whose nx_block_size field has to be recovered
// before the real geometry is known) and by callers that only handle 4 KiB
// images. It is never used as a silent fallback for images that declare
// another block size.
var DefaultGeometry = Geometry{blockSize: NX_DEFAULT_BLOCK_SIZE}

// Block geometry validation errors.
var (
	ErrBlockSizeZero        = errors.New("block size is zero")
	ErrBlockSizeTooSmall    = fmt.Errorf("block size is smaller than the minimum %#x", NX_MINIMUM_BLOCK_SIZE)
	ErrBlockSizeTooLarge    = fmt.Errorf("block size is larger than the maximum %#x", NX_MAXIMUM_BLOCK_SIZE)
	ErrBlockSizeUnaligned   = errors.New("block size is not a power of two")
	ErrBlockAddressOverflow = errors.New("physical block address overflows the address space")
)

// NewGeometry validates blockSize (as declared by nx_superblock_t) and returns
// the immutable geometry to use for every address conversion of the
// container. Declarations outside the supported range or that are not a
// power of two are rejected here rather than clamped or rounded.
func NewGeometry(blockSize uint32) (Geometry, error) {
	switch {
	case blockSize == 0:
		return Geometry{}, ErrBlockSizeZero
	case blockSize < NX_MINIMUM_BLOCK_SIZE:
		return Geometry{}, ErrBlockSizeTooSmall
	case blockSize > NX_MAXIMUM_BLOCK_SIZE:
		return Geometry{}, ErrBlockSizeTooLarge
	case blockSize&(blockSize-1) != 0:
		return Geometry{}, ErrBlockSizeUnaligned
	}
	return Geometry{blockSize: uint64(blockSize)}, nil
}

// BlockSize returns the container's physical block size in bytes. The zero
// Geometry behaves as the 4 KiB default so that structures built without
// going through NewAPFS (for example in tests) keep working.
func (g Geometry) BlockSize() uint64 {
	if g.blockSize == 0 {
		return NX_DEFAULT_BLOCK_SIZE
	}
	return g.blockSize
}

// BlockOffset converts a physical block number to a byte offset inside the
// container. It rejects addresses whose product would overflow a signed
// offset instead of silently wrapping.
func (g Geometry) BlockOffset(blockAddr uint64) (int64, error) {
	hi, lo := bits.Mul64(blockAddr, g.BlockSize())
	if hi != 0 || lo > math.MaxInt64 {
		return 0, fmt.Errorf("block address %#x: %w", blockAddr, ErrBlockAddressOverflow)
	}
	return int64(lo), nil
}
