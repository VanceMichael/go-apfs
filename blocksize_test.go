package apfs

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/blacktop/go-apfs/types"
)

// Synthetic APFS container fixtures.
//
// Every test in this file builds a minimal but valid APFS container image in
// memory at a chosen physical block size, so that geometry isolation can be
// exercised without depending on a macOS-created image. The layout is:
//
//	block 0  container superblock (nx)
//	block 1  checkpointed container superblock
//	block 3  container object map (omap_phys)
//	block 4  container omap B-tree root: volume oid -> block 5
//	block 5  volume superblock (apsb)
//	block 6  file-system object map (omap_phys)
//	block 7  FS omap B-tree root: root tree oid -> block 8
//	block 8  FS root tree (directory records, inodes, extents, xattrs)
//	block 9  regular file data
//	block 10 decmpfs compressed resource fork
const (
	blkContainerOMapObj = iota + 3
	blkContainerOMapTree
	blkVolume
	blkFSOMapObj
	blkFSOMapTree
	blkFSTree
	blkFileData
	blkRsrcData
)

const (
	imgBlocks = 11

	volOID      = uint64(16)
	rootTreeOID = uint64(203)
	rsrcOID     = uint64(99)
	dirOID      = uint64(50)
	nestedOID   = uint64(51)
	fileOID     = uint64(30)
	cmpOID      = uint64(40)
)

// memDevice is an in-memory disk.Device backed by a raw container image.
type memDevice struct {
	data []byte
	br   *bytes.Reader
}

func newMemDevice(data []byte) *memDevice {
	return &memDevice{data: data, br: bytes.NewReader(data)}
}

func (m *memDevice) ReadAt(p []byte, off int64) (int, error) { return m.br.ReadAt(p, off) }
func (m *memDevice) Close() error                            { return nil }
func (m *memDevice) GetSize() uint64                         { return uint64(len(m.data)) }

func (m *memDevice) ReadFile(w *bufio.Writer, off, length int64) error {
	if off < 0 || length < 0 || off > int64(len(m.data)) || off+length > int64(len(m.data)) {
		return fmt.Errorf("read out of range: off=%#x length=%#x device=%#x", off, length, len(m.data))
	}
	if _, err := io.CopyN(w, io.NewSectionReader(m.br, off, length), length); err != nil {
		return err
	}
	return w.Flush()
}

// putChecksum fills the object checksum field (the first eight bytes,
// expected to be zero) with the Fletcher-64 checksum of the block.
func putChecksum(block []byte) {
	sum := types.CreateChecksum(block)
	binary.LittleEndian.PutUint64(block[:8], sum)
}

// objBlock assembles a checksummed object block of bs bytes from a body that
// starts immediately after obj_phys_t.
func objBlock(bs uint32, oid, xid uint64, typ, subtype uint32, body []byte) []byte {
	block := make([]byte, bs)
	copy(block[32:], body)
	binary.LittleEndian.PutUint64(block[8:16], oid)
	binary.LittleEndian.PutUint64(block[16:24], xid)
	binary.LittleEndian.PutUint32(block[24:28], typ)
	binary.LittleEndian.PutUint32(block[28:32], subtype)
	putChecksum(block)
	return block
}

func mustStructBytes(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
		t.Fatalf("failed to encode %T: %v", v, err)
	}
	return buf.Bytes()
}

// nodeKV is one raw (key, value) pair placed in a B-tree node.
type nodeKV struct {
	key []byte
	val []byte
}

// btreeNodeBlock assembles a root leaf B-tree node. The table of contents
// uses kvloc_t (non fixed-KV) entries, which is the layout the parser expects
// when inode/dir-record xfield blobs are present.
func btreeNodeBlock(bs uint32, oid, xid uint64, subtype uint32, kvs []nodeKV) []byte {
	block := make([]byte, bs)

	// btree_node_phys_t follows obj_phys_t at offset 32 and is 24 bytes, so
	// the table of contents starts at absolute offset 56. The parser resolves
	// key addresses as keyOff + TableSpace.Len + 56; TableSpace.Len stays 0.
	binary.LittleEndian.PutUint16(block[32:34], uint16(types.BTNODE_ROOT|types.BTNODE_LEAF))
	binary.LittleEndian.PutUint16(block[34:36], 0) // level
	binary.LittleEndian.PutUint32(block[36:40], uint32(len(kvs)))

	const (
		tocStart   = 56
		tocEntry   = 8 // kvloc_t: key off/len, val off/len
		keyArea    = 256
		infoSize   = 40 // btree_info_t at the end of every root node
		rootAdjust = 40 // subtracted from value offsets on root nodes
	)

	keyCursor := keyArea
	valCursor := int(bs) - infoSize

	var totalVal int
	for _, kv := range kvs {
		totalVal += len(kv.val)
	}
	if keyArea+keyBytes(kvs) > int(bs)-infoSize-totalVal {
		panic("synthetic B-tree node overflows its block")
	}

	for i, kv := range kvs {
		// Table of contents entry.
		toc := tocStart + tocEntry*i
		binary.LittleEndian.PutUint16(block[toc:toc+2], uint16(keyCursor-tocStart))
		binary.LittleEndian.PutUint16(block[toc+2:toc+4], uint16(len(kv.key)))
		valCursor -= len(kv.val)
		binary.LittleEndian.PutUint16(block[toc+4:toc+6], uint16(int(bs)-rootAdjust-valCursor))
		binary.LittleEndian.PutUint16(block[toc+6:toc+8], uint16(len(kv.val)))

		copy(block[keyCursor:], kv.key)
		keyCursor += len(kv.key)
		copy(block[valCursor:], kv.val)
	}

	// btree_info_t at the final 40 bytes.
	info := int(bs) - infoSize
	binary.LittleEndian.PutUint32(block[info+4:info+8], bs) // node size
	binary.LittleEndian.PutUint64(block[info+16:info+24], uint64(len(kvs)))

	return objBlock(bs, oid, xid, uint32(types.OBJECT_TYPE_BTREE), subtype, block[32:])
}

func keyBytes(kvs []nodeKV) int {
	n := 0
	for _, kv := range kvs {
		n += len(kv.key)
	}
	return n
}

func jKey(oid uint64, typ uint64) uint64 {
	return oid | typ<<types.OBJ_TYPE_SHIFT
}

// fsRecordOrder returns the ordering the parser's tree walk assumes:
// records are grouped by the low 60-bit object id; ties break on the full
// key (type first) and then on remaining key bytes.
func fsRecordOrder(a, b []byte) int {
	ai := binary.LittleEndian.Uint64(a[0:8]) & types.OBJ_ID_MASK
	bi := binary.LittleEndian.Uint64(b[0:8]) & types.OBJ_ID_MASK
	if ai != bi {
		if ai < bi {
			return -1
		}
		return 1
	}
	return bytes.Compare(a, b)
}

// --- file-system record encoders -------------------------------------------------

func dirRecKV(parent, fileID uint64, name string, dir bool) nodeKV {
	flags := uint16(types.DT_REG)
	if dir {
		flags = uint16(types.DT_DIR)
	}
	key := make([]byte, 8+4+len(name))
	binary.LittleEndian.PutUint64(key[0:8], jKey(parent, uint64(types.APFS_TYPE_DIR_REC)))
	binary.LittleEndian.PutUint32(key[8:12], uint32(len(name)))
	copy(key[12:], name)

	val := make([]byte, 18) // j_drec_val_t (Flags is uint16) without xfield blob
	binary.LittleEndian.PutUint64(val[0:8], fileID)
	binary.LittleEndian.PutUint16(val[16:18], flags)
	return nodeKV{key, val}
}

func inodeKV(oid, parent uint64, name string, mode uint16, dstream types.JDstreamT, compressed bool, uncompressedSize uint64) nodeKV {
	key := make([]byte, 8)
	binary.LittleEndian.PutUint64(key, jKey(oid, uint64(types.APFS_TYPE_INODE)))

	namePad := (8 - len(name)%8) % 8
	usedData := len(name) + namePad + 40

	// j_inode_val_t is 92 bytes (InternalFlags is uint64).
	val := make([]byte, 92)
	binary.LittleEndian.PutUint64(val[0:8], parent)
	binary.LittleEndian.PutUint64(val[8:16], oid)
	var internalFlags uint64
	if compressed {
		internalFlags = uint64(types.INODE_HAS_UNCOMPRESSED_SIZE)
	}
	binary.LittleEndian.PutUint64(val[48:56], internalFlags)
	binary.LittleEndian.PutUint16(val[80:82], mode)
	binary.LittleEndian.PutUint64(val[84:92], uncompressedSize)

	// xf_blob_t, then the full x_field_t header array, then field data.
	blob := make([]byte, 4+8+len(name)+namePad+40)
	binary.LittleEndian.PutUint16(blob[0:2], 2) // xf_num_exts
	binary.LittleEndian.PutUint16(blob[2:4], uint16(usedData))
	// headers
	binary.LittleEndian.PutUint16(blob[6:8], uint16(len(name))) // NAME
	blob[4] = byte(types.INO_EXT_TYPE_NAME)
	binary.LittleEndian.PutUint16(blob[10:12], 40) // DSTREAM
	blob[8] = byte(types.INO_EXT_TYPE_DSTREAM)
	// data
	off := 12
	copy(blob[off:], name)
	off += len(name) + namePad
	binary.LittleEndian.PutUint64(blob[off:off+8], dstream.Size)
	binary.LittleEndian.PutUint64(blob[off+8:off+16], dstream.AllocedSize)
	binary.LittleEndian.PutUint64(blob[off+16:off+24], dstream.DefaultCryptoID)
	binary.LittleEndian.PutUint64(blob[off+24:off+32], dstream.TotalBytesWritten)
	binary.LittleEndian.PutUint64(blob[off+32:off+40], dstream.TotalBytesRead)

	return nodeKV{key, append(val, blob...)}
}

func extentKV(oid, logical, phys, length uint64) nodeKV {
	key := make([]byte, 16)
	binary.LittleEndian.PutUint64(key[0:8], jKey(oid, uint64(types.APFS_TYPE_FILE_EXTENT)))
	binary.LittleEndian.PutUint64(key[8:16], logical)

	val := make([]byte, 24) // j_file_extent_val_t
	binary.LittleEndian.PutUint64(val[0:8], length&types.J_FILE_EXTENT_LEN_MASK)
	binary.LittleEndian.PutUint64(val[8:16], phys)
	return nodeKV{key, val}
}

func xattrEmbeddedKV(oid uint64, name string, data []byte) nodeKV {
	key := make([]byte, 8+2+len(name))
	binary.LittleEndian.PutUint64(key[0:8], jKey(oid, uint64(types.APFS_TYPE_XATTR)))
	binary.LittleEndian.PutUint16(key[8:10], uint16(len(name)))
	copy(key[10:], name)

	val := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint16(val[0:2], uint16(types.XATTR_DATA_EMBEDDED))
	binary.LittleEndian.PutUint16(val[2:4], uint16(len(data)))
	copy(val[4:], data)
	return nodeKV{key, val}
}

func xattrStreamKV(oid, xattrOID uint64, name string) nodeKV {
	key := make([]byte, 8+2+len(name))
	binary.LittleEndian.PutUint64(key[0:8], jKey(oid, uint64(types.APFS_TYPE_XATTR)))
	binary.LittleEndian.PutUint16(key[8:10], uint16(len(name)))
	copy(key[10:], name)

	val := make([]byte, 4+48) // flags, data len, j_xattr_dstream_t
	binary.LittleEndian.PutUint16(val[0:2], uint16(types.XATTR_DATA_STREAM))
	binary.LittleEndian.PutUint16(val[2:4], 48)
	binary.LittleEndian.PutUint64(val[4:12], xattrOID)
	return nodeKV{key, val}
}

// buildRsrc wraps a single zlib chunk in an Apple compressed-metadata resource
// fork as consumed by DecmpfsDiskHeader.DecompressFile (CMP_RSRC_ZLIB).
func buildRsrc(t *testing.T, plain []byte) []byte {
	t.Helper()
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	chunk := zbuf.Bytes()

	const (
		headerSize = 0x100
		// First chunk offset is relative to headerSize+4; 12 puts it just
		// past the 8-byte offset-table entry.
		firstOffset = 12
		chunkStart  = headerSize + 4 + firstOffset // 0x110
	)
	total := chunkStart + len(chunk)
	buf := make([]byte, total)

	binary.BigEndian.PutUint32(buf[0:4], headerSize)
	binary.BigEndian.PutUint32(buf[4:8], uint32(total))
	binary.BigEndian.PutUint32(buf[8:12], headerSize) // data size
	binary.BigEndian.PutUint32(buf[12:16], 0)         // flags

	binary.BigEndian.PutUint32(buf[headerSize:headerSize+4], headerSize)
	binary.LittleEndian.PutUint32(buf[headerSize+4:headerSize+8], 1) // num blocks
	binary.LittleEndian.PutUint32(buf[headerSize+8:headerSize+12], firstOffset)
	binary.LittleEndian.PutUint32(buf[headerSize+12:headerSize+16], uint32(len(chunk)))
	copy(buf[chunkStart:], chunk)
	return buf
}

// imageSpec parameterizes a synthetic container.
type imageSpec struct {
	blockSize uint32 // physical block size used for the image

	plain    []byte // regular file payload
	cmpPlain []byte // uncompressed payload of the compressed file
	rsrc     []byte // compressed resource fork bytes

	declaredBS   uint32 // nx_block_size as declared by block 0 (defaults to blockSize)
	blockCount   uint64 // nx_block_count (defaults to imgBlocks)
	descBase     uint64 // checkpoint descriptor base (defaults to 1)
	descBlocks   uint32 // checkpoint descriptor length (defaults to 1)
	cpDeclaredBS uint32 // block size declared by the checkpoint superblock
	block0Type   uint32 // object type of block 0 (defaults to NX_SUPERBLOCK)
	block0Magic  string // magic at block 0 (defaults to NXSB)
	cpBody       []byte // override checkpoint block body
	deviceLen    int    // override device length in bytes
}

// buildImage assembles the raw container bytes for spec.
func buildImage(t *testing.T, spec imageSpec) []byte {
	t.Helper()

	bs := spec.blockSize
	if spec.declaredBS == 0 {
		spec.declaredBS = bs
	}
	if spec.blockCount == 0 {
		spec.blockCount = imgBlocks
	}
	if spec.descBase == 0 {
		spec.descBase = 1
	}
	if spec.descBlocks == 0 {
		spec.descBlocks = 1
	}
	if spec.cpDeclaredBS == 0 {
		spec.cpDeclaredBS = spec.declaredBS
	}
	if spec.block0Type == 0 {
		spec.block0Type = uint32(types.OBJECT_TYPE_NX_SUPERBLOCK)
	}
	if spec.block0Magic == "" {
		spec.block0Magic = types.NX_MAGIC
	}

	nxBody := func(declaredBlockSize uint32) []byte {
		nx := types.NxSuperblockT{
			Magic:          [4]byte{'N', 'X', 'S', 'B'},
			BlockSize:      declaredBlockSize,
			BlockCount:     spec.blockCount,
			XpDescBlocks:   spec.descBlocks,
			XpDescBase:     spec.descBase,
			XpDescIndex:    0,
			XpDescLen:      spec.descBlocks,
			OmapOid:        blkContainerOMapObj,
			MaxFileSystems: types.NX_MAX_FILE_SYSTEMS,
		}
		return mustStructBytes(t, nx)
	}

	block0Body := nxBody(spec.declaredBS)
	// Optionally corrupt the magic after the fact (PeekContainerGeometry
	// checks it before checksum verification).
	if spec.block0Magic != types.NX_MAGIC {
		copy(block0Body[0:4], spec.block0Magic)
	}
	block0 := objBlock(bs, 1, 9, spec.block0Type, 0, block0Body)

	cpBody := spec.cpBody
	if cpBody == nil {
		cpBody = nxBody(spec.cpDeclaredBS)
	}
	block1 := objBlock(bs, 1, 10, uint32(types.OBJECT_TYPE_NX_SUPERBLOCK), 0, cpBody)

	omapObj := mustStructBytes(t, types.OMapPhysT{
		TreeType: types.OBJECT_TYPE_BTREE,
		TreeOid:  blkContainerOMapTree,
	})
	block3 := objBlock(bs, blkContainerOMapObj, 9, uint32(types.OBJECT_TYPE_OMAP), 0, omapObj)

	containerOMapNode := btreeNodeBlock(bs, blkContainerOMapTree, 9, uint32(types.OBJECT_TYPE_OMAP), []nodeKV{
		omapKV(volOID, 10, blkVolume),
	})

	vol := types.ApfsSuperblockT{
		Magic:        [4]byte{'A', 'P', 'S', 'B'},
		OmapOid:      blkFSOMapObj,
		RootTreeOid:  types.OidT(rootTreeOID),
		RootTreeType: types.OBJECT_TYPE_FSTREE,
	}
	copy(vol.VolumeName[:], "TestVol")
	block5 := objBlock(bs, volOID, 10, uint32(types.OBJECT_TYPE_FS), 0, mustStructBytes(t, vol))

	fsOmapObj := mustStructBytes(t, types.OMapPhysT{
		TreeType: types.OBJECT_TYPE_BTREE,
		TreeOid:  blkFSOMapTree,
	})
	block6 := objBlock(bs, blkFSOMapObj, 10, uint32(types.OBJECT_TYPE_OMAP), 0, fsOmapObj)

	fsOMapNode := btreeNodeBlock(bs, blkFSOMapTree, 10, uint32(types.OBJECT_TYPE_OMAP), []nodeKV{
		omapKV(rootTreeOID, 10, blkFSTree),
	})

	// File-system records.
	kvs := []nodeKV{
		// root directory (oid FSROOT_OID)
		dirRecKV(types.FSROOT_OID, dirOID, "d", true),
		dirRecKV(types.FSROOT_OID, cmpOID, "data.z", false),
		dirRecKV(types.FSROOT_OID, fileOID, "hello.txt", false),
		// regular file
		inodeKV(fileOID, types.FSROOT_OID, "hello.txt", 0o100644,
			types.JDstreamT{Size: uint64(len(spec.plain)), AllocedSize: uint64(len(spec.plain)), TotalBytesWritten: uint64(len(spec.plain))},
			false, 0),
		extentKV(fileOID, 0, blkFileData, uint64(len(spec.plain))),
		// compressed file (zlib resource fork, CMP_RSRC_ZLIB)
		inodeKV(cmpOID, types.FSROOT_OID, "data.z", 0o100644,
			types.JDstreamT{}, true, uint64(len(spec.cmpPlain))),
		xattrStreamKV(cmpOID, rsrcOID, types.XATTR_RESOURCEFORK_EA_NAME),
		xattrEmbeddedKV(cmpOID, types.XATTR_DECMPFS_EA_NAME, decmpfsHeader(uint32(types.CMP_RSRC_ZLIB), uint64(len(spec.cmpPlain)))),
		extentKV(rsrcOID, 0, blkRsrcData, uint64(len(spec.rsrc))),
		// nested directory and regular file inside it
		inodeKV(dirOID, types.FSROOT_OID, "d", 0o040755, types.JDstreamT{}, false, 0),
		dirRecKV(dirOID, nestedOID, "nested.txt", false),
		inodeKV(nestedOID, dirOID, "nested.txt", 0o100644,
			types.JDstreamT{Size: uint64(len(spec.plain)), AllocedSize: uint64(len(spec.plain)), TotalBytesWritten: uint64(len(spec.plain))},
			false, 0),
		extentKV(nestedOID, 0, blkFileData, uint64(len(spec.plain))),
	}
	sort.Slice(kvs, func(i, j int) bool {
		return fsRecordOrder(kvs[i].key, kvs[j].key) < 0
	})
	block8 := btreeNodeBlock(bs, rootTreeOID, 10, uint32(types.OBJECT_TYPE_FSTREE), kvs)

	blocks := map[uint64][]byte{
		0:                    block0,
		1:                    block1,
		blkContainerOMapObj:  block3,
		blkContainerOMapTree: containerOMapNode,
		blkVolume:            block5,
		blkFSOMapObj:         block6,
		blkFSOMapTree:        fsOMapNode,
		blkFSTree:            block8,
		blkFileData:          rawBlock(bs, spec.plain),
		blkRsrcData:          rawBlock(bs, spec.rsrc),
	}

	deviceLen := imgBlocks * int(bs)
	if spec.deviceLen != 0 {
		deviceLen = spec.deviceLen
	}
	image := make([]byte, deviceLen)
	for addr, data := range blocks {
		off := int(addr) * int(bs)
		if off+len(data) > deviceLen {
			// Truncated-device fixtures intentionally hold partial blocks.
			n := deviceLen - off
			if n <= 0 {
				continue
			}
			data = data[:n]
		}
		copy(image[off:], data)
	}
	return image
}

func omapKV(oid, xid, paddr uint64) nodeKV {
	key := make([]byte, 16)
	binary.LittleEndian.PutUint64(key[0:8], oid)
	binary.LittleEndian.PutUint64(key[8:16], xid)
	val := make([]byte, 16) // omap_val_t
	binary.LittleEndian.PutUint32(val[0:4], 0)
	binary.LittleEndian.PutUint32(val[4:8], 0) // size
	binary.LittleEndian.PutUint64(val[8:16], paddr)
	return nodeKV{key, val}
}

func decmpfsHeader(typ uint32, uncompressed uint64) []byte {
	buf := make([]byte, 16)
	copy(buf[0:4], "cmpf")
	binary.LittleEndian.PutUint32(buf[4:8], typ)
	binary.LittleEndian.PutUint64(buf[8:16], uncompressed)
	return buf
}

func rawBlock(bs uint32, payload []byte) []byte {
	block := make([]byte, bs)
	copy(block, payload)
	return block
}

// validSpec returns a working image specification for blockSize.
func validSpec(t *testing.T, blockSize uint32, tag string) imageSpec {
	plain := []byte(strings.Repeat("regular payload of the "+tag+" container ", 3))
	cmpPlain := []byte(strings.Repeat("compressed payload unique to the "+tag+" container; ", 4))
	return imageSpec{
		blockSize: blockSize,
		plain:     plain,
		cmpPlain:  cmpPlain,
		rsrc:      buildRsrc(t, cmpPlain),
	}
}

func mustMount(t *testing.T, image []byte) *APFS {
	t.Helper()
	a, err := NewAPFS(newMemDevice(image))
	if err != nil {
		t.Fatalf("failed to mount image: %v", err)
	}
	return a
}

// stdoutMu serializes tests that temporarily redirect os.Stdout.
var stdoutMu sync.Mutex

func captureStdout(fn func() error) (string, error) {
	stdoutMu.Lock()
	defer stdoutMu.Unlock()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	os.Stdout = w
	runErr := fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

// --- types-level checks -----------------------------------------------------------

func TestChecksumVerification(t *testing.T) {
	for _, bs := range []uint32{4096, 8192, 16384, 65536} {
		block := make([]byte, bs)
		for i := range block {
			block[i] = byte(i * 7)
		}
		for i := 0; i < 8; i++ {
			block[i] = 0
		}
		putChecksum(block)
		if !types.VerifyChecksum(block) {
			t.Fatalf("block size %#x: checksum failed to verify", bs)
		}
		block[100] ^= 0xff
		if types.VerifyChecksum(block) {
			t.Fatalf("block size %#x: tampered block verified", bs)
		}
	}
}

func TestGeometryValidation(t *testing.T) {
	for _, bs := range []uint32{0, 0x200, 0x800, 0x1800, 0x1001, 0x20000} {
		if _, err := types.NewGeometry(bs); err == nil {
			t.Fatalf("block size %#x accepted as valid geometry", bs)
		}
	}
	for _, bs := range []uint32{0x1000, 0x2000, 0x4000, 0x10000} {
		g, err := types.NewGeometry(bs)
		if err != nil {
			t.Fatalf("block size %#x rejected: %v", bs, err)
		}
		if g.BlockSize() != uint64(bs) {
			t.Fatalf("block size = %#x, want %#x", g.BlockSize(), bs)
		}
		off, err := g.BlockOffset(7)
		if err != nil || off != int64(7*uint64(bs)) {
			t.Fatalf("BlockOffset(7) = %#x, %v; want %#x", off, err, 7*uint64(bs))
		}
	}
}

// --- functional checks for default and non-default block sizes --------------------

func TestNonDefaultBlockSizeMount(t *testing.T) {
	for _, bs := range []uint32{types.NX_DEFAULT_BLOCK_SIZE, 0x2000, 0x4000, 0x10000} {
		t.Run(fmt.Sprintf("%#x", bs), func(t *testing.T) {
			spec := validSpec(t, bs, fmt.Sprintf("%d-byte-block", bs))
			a := mustMount(t, buildImage(t, spec))
			defer a.Close()

			if got := a.geo.BlockSize(); got != uint64(bs) {
				t.Fatalf("instance geometry = %#x, want %#x", got, bs)
			}

			// ls: the volume root lists the regular file, compressed file
			// and directory.
			out, err := captureStdout(func() error { return a.List("/") })
			if err != nil {
				t.Fatalf("List failed: %v", err)
			}
			for _, name := range []string{"hello.txt", "data.z", "d"} {
				if !strings.Contains(out, name) {
					t.Fatalf("List(/) = %q, missing %q", out, name)
				}
			}
			out, err = captureStdout(func() error { return a.List("/d/nested.txt") })
			if err != nil {
				t.Fatalf("List nested file failed: %v", err)
			}
			if !strings.Contains(out, "nested.txt") {
				t.Fatalf("List(/d/nested.txt) = %q, missing nested.txt", out)
			}

			// oid: metadata lookup for the regular file oid.
			if _, err := captureStdout(func() error { return a.OidInfo(fileOID) }); err != nil {
				t.Fatalf("OidInfo failed: %v", err)
			}

			// cat: plain and compressed payloads.
			out, err = captureStdout(func() error { return a.Cat("/hello.txt") })
			if err != nil || out != string(spec.plain) {
				t.Fatalf("Cat regular file = %q, err %v; want %q", out, err, spec.plain)
			}
			out, err = captureStdout(func() error { return a.Cat("/data.z") })
			if err != nil || out != string(spec.cmpPlain) {
				t.Fatalf("Cat compressed file = %q, err %v; want %q", out, err, spec.cmpPlain)
			}

			// cp: full extraction, regular, compressed and nested files.
			dest := t.TempDir()
			if err := a.Copy("/", dest); err != nil {
				t.Fatalf("Copy failed: %v", err)
			}
			for path, want := range map[string][]byte{
				"hello.txt":    spec.plain,
				"data.z":       spec.cmpPlain,
				"d/nested.txt": spec.plain,
			} {
				got, err := os.ReadFile(filepath.Join(dest, path))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s = %q, err %v; want %q", path, got, err, want)
				}
			}
		})
	}
}

// TestGeometryIsolationInterleaved keeps two containers with different block
// sizes open at once and interleaves metadata and data reads. With the old
// global BLOCK_SIZE one instance's regular-file and resource-fork extents
// were addressed with the other instance's multiplier.
func TestGeometryIsolationInterleaved(t *testing.T) {
	spec4 := validSpec(t, 0x1000, "4k")
	spec16 := validSpec(t, 0x4000, "16k")
	img4, img16 := buildImage(t, spec4), buildImage(t, spec16)

	a := mustMount(t, img4)
	b := mustMount(t, img16)
	defer a.Close()
	defer b.Close()

	if a.geo.BlockSize() == b.geo.BlockSize() {
		t.Fatalf("instances share block geometry")
	}

	const goroutines, iterations = 8, 6

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// Alternate which container is touched first; every read is
				// repeated on both instances.
				first, second := a, b
				firstSpec, secondSpec := spec4, spec16
				if (g+i)%2 == 0 {
					first, second = b, a
					firstSpec, secondSpec = spec16, spec4
				}

				dest1 := filepath.Join(t.TempDir(), fmt.Sprintf("g%d-i%d-a", g, i))
				if err := first.Copy("/", dest1); err != nil {
					errCh <- fmt.Errorf("copy first: %w", err)
					return
				}
				if got, err := os.ReadFile(filepath.Join(dest1, "hello.txt")); err != nil || !bytes.Equal(got, firstSpec.plain) {
					errCh <- fmt.Errorf("first regular payload mismatch (%v): %q", err, got)
					return
				}
				if got, err := os.ReadFile(filepath.Join(dest1, "data.z")); err != nil || !bytes.Equal(got, firstSpec.cmpPlain) {
					errCh <- fmt.Errorf("first compressed payload mismatch (%v): %q", err, got)
					return
				}

				// Metadata traversal (oid) interleaved with reads of the other
				// instance; output goes to the shared os.Stdout and is only
				// checked for errors.
				if err := second.OidInfo(fileOID); err != nil {
					errCh <- fmt.Errorf("second oid: %w", err)
					return
				}

				dest2 := filepath.Join(t.TempDir(), fmt.Sprintf("g%d-i%d-b", g, i))
				if err := second.Copy("/d", dest2); err != nil {
					errCh <- fmt.Errorf("second dir copy: %w", err)
					return
				}
				if got, err := os.ReadFile(filepath.Join(dest2, "nested.txt")); err != nil || !bytes.Equal(got, secondSpec.plain) {
					errCh <- fmt.Errorf("second nested payload mismatch (%v): %q", err, got)
					return
				}
				// Resource-fork decompression must read the extent at the
				// second instance's own block multiplier.
				dest3 := filepath.Join(t.TempDir(), fmt.Sprintf("g%d-i%d-c", g, i))
				if err := second.Copy("/data.z", dest3); err != nil {
					errCh <- fmt.Errorf("second compressed copy: %w", err)
					return
				}
				if got, err := os.ReadFile(filepath.Join(dest3, "data.z")); err != nil || !bytes.Equal(got, secondSpec.cmpPlain) {
					errCh <- fmt.Errorf("second compressed payload mismatch (%v): %q", err, got)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// --- rejection of invalid declarations ---------------------------------------------

func TestInvalidGeometryRejected(t *testing.T) {
	good := validSpec(t, 0x1000, "4k")

	// A truncated 8 KiB block: checksummed as 4 KiB, then padded into a
	// device whose geometry claims 8 KiB blocks.
	paddedSpec := good
	paddedSpec.blockSize = 0x2000
	paddedSpec.declaredBS = 0x2000
	paddedSpec.blockCount = 2
	paddedSpec.deviceLen = 2 * 0x2000
	paddedImage := buildImage(t, paddedSpec)
	// Rebuild block 0 so its checksum is valid only over the first 4 KiB:
	// under the declared 8 KiB geometry verification must fail.
	head := make([]byte, 0x2000)
	copy(head, paddedImage[:0x1000])
	binary.LittleEndian.PutUint32(head[36:40], 0x2000)
	for i := 0; i < 8; i++ {
		head[i] = 0
	}
	putChecksum(head[:0x1000]) // checksum over the 4 KiB prefix only
	copy(paddedImage, head)

	// Checkpoint block whose own declared block size disagrees with block 0
	// while its checksum is valid for the 4 KiB read length.
	mismatchSpec := good
	mismatchSpec.cpDeclaredBS = 0x2000
	mismatchImage := buildImage(t, mismatchSpec)

	for _, tc := range []struct {
		name  string
		image []byte
	}{
		{"block size below minimum", buildImage(t, mutate(good, func(s *imageSpec) { s.declaredBS = 0x800 }))},
		{"block size above maximum", buildImage(t, mutate(good, func(s *imageSpec) { s.declaredBS = 0x20000 }))},
		{"block size not power of two", buildImage(t, mutate(good, func(s *imageSpec) { s.declaredBS = 0x1800 }))},
		{"declared block larger than device", buildImage(t, mutate(good, func(s *imageSpec) {
			s.declaredBS = 0x2000
			s.deviceLen = 0x1000
		}))},
		{"declared container larger than device", buildImage(t, mutate(good, func(s *imageSpec) {
			s.declaredBS = 0x2000
			s.blockCount = 1 << 20
		}))},
		{"non-superblock object at block zero", buildImage(t, mutate(good, func(s *imageSpec) { s.block0Type = uint32(types.OBJECT_TYPE_FS) }))},
		{"bad superblock magic", buildImage(t, mutate(good, func(s *imageSpec) { s.block0Magic = "XXXX" }))},
		{"checksum inconsistent with 8 KiB geometry", paddedImage},
		{"checkpoint descriptor out of bounds", buildImage(t, mutate(good, func(s *imageSpec) { s.descBase = 100 }))},
		{"checkpoint geometry disagrees", mismatchImage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := NewAPFS(newMemDevice(tc.image))
			if err == nil {
				a.Close()
				t.Fatalf("invalid image mounted successfully")
			}
			if a != nil {
				t.Fatalf("failed mount returned a visible container instance")
			}
		})
	}
}

func mutate(spec imageSpec, fn func(*imageSpec)) imageSpec {
	fn(&spec)
	return spec
}

// TestFailedMountIsolation keeps a valid container open while malformed
// containers fail to mount; neither the open instance nor later mounts may be
// affected.
func TestFailedMountIsolation(t *testing.T) {
	spec4 := validSpec(t, 0x1000, "4k")
	spec8 := validSpec(t, 0x2000, "8k")

	open4 := mustMount(t, buildImage(t, spec4))
	defer open4.Close()

	invalid := []imageSpec{
		mutate(spec4, func(s *imageSpec) { s.declaredBS = 0x800 }),
		mutate(spec4, func(s *imageSpec) { s.descBase = 999 }),
		mutate(spec8, func(s *imageSpec) { s.declaredBS = 0x1800 }),
	}
	for i, spec := range invalid {
		if a, err := NewAPFS(newMemDevice(buildImage(t, spec))); err == nil {
			a.Close()
			t.Fatalf("invalid image %d mounted", i)
		}
		// The already-open instance keeps reading from its own offsets.
		dest := filepath.Join(t.TempDir(), fmt.Sprintf("open-%d", i))
		if err := open4.Copy("/", dest); err != nil {
			t.Fatalf("open instance broken by failed mount %d: %v", i, err)
		}
		got, err := os.ReadFile(filepath.Join(dest, "data.z"))
		if err != nil || !bytes.Equal(got, spec4.cmpPlain) {
			t.Fatalf("open instance compressed data corrupted after failed mount %d: %q (%v)", i, got, err)
		}
	}

	// A fresh valid mount, repeatedly interleaved with failures.
	for round := 0; round < 3; round++ {
		if a, err := NewAPFS(newMemDevice(buildImage(t, invalid[round%len(invalid)]))); err == nil {
			a.Close()
			t.Fatalf("invalid image mounted on round %d", round)
		}
		fresh := mustMount(t, buildImage(t, spec8))
		out, err := captureStdout(func() error { return fresh.Cat("/hello.txt") })
		if err != nil || out != string(spec8.plain) {
			fresh.Close()
			t.Fatalf("fresh 8 KiB mount wrong on round %d: %q (%v)", round, out, err)
		}
		fresh.Close()
	}
}

// TestConcurrentClose hammers Close from many goroutines and repeats it: the
// underlying device is closed exactly once and every caller gets the same
// result.
func TestConcurrentClose(t *testing.T) {
	spec := validSpec(t, 0x2000, "8k")
	path := filepath.Join(t.TempDir(), "container.raw")
	if err := os.WriteFile(path, buildImage(t, spec), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const n = 64
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = a.Close()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Close[%d] returned %v", i, err)
		}
	}
	// Repeated close after the stampede stays safe.
	if err := a.Close(); err != nil {
		t.Fatalf("second Close returned %v", err)
	}

	// A failed Open must close the underlying file itself: open a valid
	// image path, then feed a malformed device through Open is not possible,
	// so check NewAPFS leaves the caller-owned device open.
	dev := newMemDevice(buildImage(t, mutate(spec, func(s *imageSpec) { s.declaredBS = 0x800 })))
	if _, err := NewAPFS(dev); err == nil {
		t.Fatal("expected malformed mount to fail")
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("caller-owned device should stay usable after failed NewAPFS: %v", err)
	}
}

// TestConcurrentMountsAndReads mixes fresh mounts, reads and closes of
// independent images to exercise geometry state under -race.
func TestConcurrentMountsAndReads(t *testing.T) {
	specs := map[uint32]imageSpec{}
	images := map[uint32][]byte{}
	for _, bs := range []uint32{0x1000, 0x2000, 0x4000} {
		s := validSpec(t, bs, fmt.Sprintf("race-%d", bs))
		specs[bs] = s
		images[bs] = buildImage(t, s)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 3*4)
	for _, bs := range []uint32{0x1000, 0x2000, 0x4000} {
		for round := 0; round < 4; round++ {
			wg.Add(1)
			go func(bs uint32) {
				defer wg.Done()
				a := mustMount(t, images[bs])
				defer a.Close()
				dest := filepath.Join(t.TempDir(), "out")
				if err := a.Copy("/", dest); err != nil {
					errCh <- err
					return
				}
				got, err := os.ReadFile(filepath.Join(dest, "data.z"))
				if err != nil || !bytes.Equal(got, specs[bs].cmpPlain) {
					errCh <- fmt.Errorf("bs %#x compressed payload: %q (%v)", bs, got, err)
				}
			}(bs)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
