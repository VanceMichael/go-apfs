package apfs

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/apex/log"
	"github.com/blacktop/go-apfs/pkg/disk"
	"github.com/blacktop/go-apfs/types"
)

// fsNodeCacheSize bounds the LRU of decoded B-tree nodes attached to the FS
// omap root: 4096 nodes is 16 MiB of 4 KiB blocks, a ceiling chosen to be
// generous rather than measured.
const fsNodeCacheSize = 4096

// APFS apple file system object
type APFS struct {
	Container   *types.NxSuperblock
	Valid       *types.NxSuperblock
	Volume      *types.ApfsSuperblock
	FSRootBtree types.BTreeNodePhys

	nxsb            *types.Obj // Container
	checkPointDesc  []*types.Obj
	validCheckPoint *types.Obj
	volume          *types.Obj
	fsOMapBtree     *types.BTreeNodePhys

	dev disk.Device
	r   io.ReaderAt

	// geo is the verified physical block geometry of this container. It is
	// established before the mount becomes visible and never changes: every
	// block-address conversion in this instance uses it. Instances never
	// share geometry, so mounting a container with another block size cannot
	// affect this one.
	geo types.Geometry

	// closer is owned by Open only; containers built with NewAPFS leave it
	// nil because the caller owns the device. closeOnce makes Close safe to
	// call concurrently and repeatedly.
	closer    io.Closer
	closeOnce sync.Once
	closeErr  error
}

// Open opens the named file using os.Open and prepares it for use as an APFS.
func Open(name string) (*APFS, error) {
	dev, err := disk.Open(name)
	if err != nil {
		return nil, err
	}
	ff, err := NewAPFS(dev)
	if err != nil {
		// NewAPFS never closes a device it did not open; the failed mount
		// must not leak the file descriptor.
		_ = dev.Close()
		return nil, err
	}
	ff.closer = dev
	return ff, nil
}

// Close closes the APFS device when the APFS was created with Open. APFS
// objects built directly with NewAPFS are not closed here because the caller
// owns the device. Close is idempotent and safe for concurrent use: exactly
// one close is performed and every caller receives the same result.
func (a *APFS) Close() error {
	a.closeOnce.Do(func() {
		if a.closer != nil {
			a.closeErr = a.closer.Close()
		}
	})
	return a.closeErr
}

// NewAPFS creates a new APFS for accessing a apple filesystem container or file in an underlying reader.
// The apfs is expected to start at position 0 in the ReaderAt.
//
// The device is not closed on failure (or by Close); its lifetime is the
// caller's responsibility.
func NewAPFS(dev disk.Device) (*APFS, error) {

	var err error

	a := new(APFS)
	a.dev = dev
	a.r = dev

	// Phase 1 — bootstrap. The container block size is a field of the
	// superblock at block 0, so it has to be recovered with a fixed 4 KiB
	// provisional read before the real geometry is known. This probe neither
	// verifies a checksum nor becomes the mount's geometry: it only extracts
	// the declared size.
	geo, boot, err := types.PeekContainerGeometry(a.r)
	if err != nil {
		return nil, fmt.Errorf("failed to read APFS container: %w", err)
	}

	// Reject geometric declarations that cannot fit the backing device
	// before any object is trusted or any mount state becomes visible.
	bs := geo.BlockSize()
	if devSize := dev.GetSize(); devSize > 0 {
		if bs > devSize {
			return nil, fmt.Errorf("declared block size %#x does not fit in a %#x byte device", bs, devSize)
		}
		if boot.BlockCount != 0 {
			hi, total := bits.Mul64(boot.BlockCount, bs)
			if hi != 0 || total > devSize {
				return nil, fmt.Errorf("declared container geometry %d blocks of %#x bytes exceeds device size %#x", boot.BlockCount, bs, devSize)
			}
		}
	}

	// Phase 2 — authoritative read of block 0 with the declared geometry.
	// The block checksum is verified over the whole block and its declared
	// block size must match the geometry; an inconsistent declaration fails
	// here instead of silently falling back to 4 KiB.
	a.nxsb, err = types.ReadObjAt(a.r, 0, geo)
	if err != nil {
		return nil, fmt.Errorf("failed to read APFS container: %w", err)
	}
	a.geo = geo

	if nxsb, ok := a.nxsb.Body.(types.NxSuperblock); ok {
		a.Container = &nxsb
	}

	if bs != types.NX_DEFAULT_BLOCK_SIZE {
		log.Warnf("found non-standard blocksize in APFS nx_superblock_t: %#x", bs)
	}

	log.WithFields(log.Fields{
		"checksum":   fmt.Sprintf("%#x", a.nxsb.Hdr.Checksum()),
		"oid":        fmt.Sprintf("%#x", a.nxsb.Hdr.Oid),
		"xid":        fmt.Sprintf("%#x", a.nxsb.Hdr.Xid),
		"type":       a.nxsb.Hdr.GetType(),
		"sub_type":   a.nxsb.Hdr.GetSubType(),
		"flag":       a.nxsb.Hdr.GetFlag(),
		"magic":      a.Container.Magic.String(),
		"block_size": fmt.Sprintf("%#x", bs),
	}).Debug("APFS Container")

	log.WithFields(log.Fields{
		"checksum": fmt.Sprintf("%#x", a.Container.OMap.Hdr.Checksum()),
		"type":     a.Container.OMap.Hdr.GetType(),
		"oid":      fmt.Sprintf("%#x", a.Container.OMap.Hdr.Oid),
		"xid":      fmt.Sprintf("%#x", a.Container.OMap.Hdr.Xid),
		"sub_type": a.Container.OMap.Hdr.GetSubType(),
		"flag":     a.Container.OMap.Hdr.GetFlag(),
	}).Debug("Object Map")

	log.Debug("Parsing Checkpoint Description")
	if err := a.getValidCSB(); err != nil {
		return nil, fmt.Errorf("failed to find the container superblock that has the largest transaction identifier and isnʼt malformed: %v", err)
	}

	if len(a.Valid.OMap.Body.(types.OMap).Tree.Body.(types.BTreeNodePhys).Entries) == 1 { // TODO: could be more than 1 for non IPSW APFS volumes?
		if entry, ok := a.Valid.OMap.Body.(types.OMap).Tree.Body.(types.BTreeNodePhys).Entries[0].(types.OMapNodeEntry); ok {
			a.volume, err = types.ReadObjAt(a.r, uint64(entry.Val.Paddr), a.geo)
			if err != nil {
				return nil, fmt.Errorf("failed to read APFS omap.tree.entry.omap (volume): %v", err)
			}
			if vol, ok := a.volume.Body.(types.ApfsSuperblock); ok {
				log.WithFields(log.Fields{
					"checksum": fmt.Sprintf("%#x", a.volume.Hdr.Checksum()),
					"type":     a.volume.Hdr.GetType(),
					"oid":      fmt.Sprintf("%#x", a.volume.Hdr.Oid),
					"xid":      fmt.Sprintf("%#x", a.volume.Hdr.Xid),
					"sub_type": a.volume.Hdr.GetSubType(),
					"flag":     a.volume.Hdr.GetFlag(),
				}).Debug(fmt.Sprintf("APFS Volume (%s)", string(vol.VolumeName[:])))

				a.Volume = &vol
			}
		}
	}

	log.Debugf("File System OMap Btree: %s", a.Volume.OMap.Body.(types.OMap).Tree)

	if ombtree, ok := a.Volume.OMap.Body.(types.OMap).Tree.Body.(types.BTreeNodePhys); ok {
		a.fsOMapBtree = &ombtree
		if err := a.fsOMapBtree.EnableNodeCache(fsNodeCacheSize); err != nil {
			return nil, fmt.Errorf("failed to enable fs omap node cache: %w", err)
		}
	}

	fsRootEntry, err := a.fsOMapBtree.GetOMapEntry(a.r, a.Volume.RootTreeOid, a.volume.Hdr.Xid)
	if err != nil {
		return nil, fmt.Errorf("failed to get root entry: %v", err)
	}

	log.Debugf("File System Root Entry: %s", fsRootEntry)

	fsRootBtreeObj, err := types.ReadObjAt(a.r, fsRootEntry.Val.Paddr, a.geo)
	if err != nil {
		return nil, fmt.Errorf("failed to read root btree: %v", err)
	}

	if root, ok := fsRootBtreeObj.Body.(types.BTreeNodePhys); ok {
		a.FSRootBtree = root
	}

	return a, nil
}

// getValidCSB returns the container superblock that has the largest transaction identifier and isnʼt malformed
func (a *APFS) getValidCSB() error {

	nxsb := a.nxsb.Body.(types.NxSuperblock)

	if (nxsb.XpDescBlocks >> 31) != 0 {
		return fmt.Errorf("unable to parse non-contiguous checkpoint descriptor area")
	}
	xpDescBlocks := nxsb.XpDescBlocks & ^(uint32(1) << 31)
	if xpDescBlocks == 0 {
		return fmt.Errorf("container superblock declares an empty checkpoint descriptor area")
	}

	// Every checkpoint descriptor block must lie inside the declared
	// container; an out-of-bounds base or length is rejected outright.
	if nxsb.BlockCount != 0 {
		if uint64(nxsb.XpDescBase) >= nxsb.BlockCount {
			return fmt.Errorf("checkpoint descriptor base block %#x is outside the container (%d blocks)", nxsb.XpDescBase, nxsb.BlockCount)
		}
		if uint64(xpDescBlocks) > nxsb.BlockCount-uint64(nxsb.XpDescBase) {
			return fmt.Errorf("checkpoint descriptor area of %d blocks at block %#x runs past the container (%d blocks)", xpDescBlocks, nxsb.XpDescBase, nxsb.BlockCount)
		}
	}

	for i := range xpDescBlocks {
		o, err := types.ReadObjAt(a.r, nxsb.XpDescBase+uint64(i), a.geo)
		if err != nil {
			if errors.Is(err, types.ErrBadBlockChecksum) {
				log.Debug(fmt.Sprintf("checkpoint block at index %d failed checksum validation. Skipping...", i))
				continue
			}
			return fmt.Errorf("failed to read XpDescBlock: %v", err)
		}
		a.checkPointDesc = append(a.checkPointDesc, o)
	}

	if len(a.checkPointDesc) == 0 {
		return fmt.Errorf("no checkpoint descriptor block passed verification")
	}

	a.validCheckPoint = a.checkPointDesc[len(a.checkPointDesc)-1]

	cp, ok := a.validCheckPoint.Body.(types.NxSuperblock)
	if !ok {
		return fmt.Errorf("last checkpoint descriptor block is %s, not an nx_superblock_t", a.validCheckPoint.Hdr.GetType())
	}

	// The checkpointed geometry must agree with the geometry verified from
	// block 0; otherwise block addresses derived later in the mount would be
	// ambiguous.
	if cp.BlockSize != uint32(a.geo.BlockSize()) {
		return fmt.Errorf("checkpoint superblock declares block size %#x, which disagrees with block 0 block size %#x", cp.BlockSize, a.geo.BlockSize())
	}

	a.Valid = &cp

	if a.Valid.XpDescIndex+a.Valid.XpDescLen <= xpDescBlocks { // TODO: remove this
		log.Debug("contiguous")
	} else {
		log.Warn("shizzzz")
	}

	return nil
}

func (a *APFS) OidInfo(oid uint64) error {

	sr := a.r

	fsRecords, err := a.fsOMapBtree.GetFSRecordsForOid(sr, a.FSRootBtree, types.OidT(oid), types.XidT(^uint64(0)))
	if err != nil {
		return fmt.Errorf("failed to get FS records for oid %#x: %v", types.OidT(oid), err)
	}

	for _, rec := range fsRecords {
		switch rec.Hdr.GetType() {
		case types.APFS_TYPE_XATTR:
			switch rec.Key.(types.JXattrKeyT).Name {
			case types.XATTR_RESOURCEFORK_EA_NAME:
				if rec.Val.(types.JXattrValT).Flags.DataEmbedded() {
					fmt.Println(rec)
				} else if rec.Val.(types.JXattrValT).Flags.DataStream() {
					fmt.Println(rec)
					fsRecords, err = a.fsOMapBtree.GetFSRecordsForOid(sr, a.FSRootBtree, types.OidT(rec.Val.(types.JXattrValT).Data.(types.JXattrDstreamT).XattrObjID), types.XidT(^uint64(0)))
					if err != nil {
						return fmt.Errorf("failed to get fs records for dstream oid %#x: %v", types.OidT(rec.Val.(types.JXattrValT).Data.(types.JXattrDstreamT).XattrObjID), err)
					}
					for _, rec := range fsRecords {
						fmt.Printf("\t%s\n", rec)
					}
				}
			case types.XATTR_SYMLINK_EA_NAME:
				fmt.Println(rec)
				fmt.Printf("\tsymlink=%s\n", types.NameColor(string(rec.Val.(types.JXattrValT).Data.([]byte)[:])))
			}
		case types.APFS_TYPE_DIR_REC:
			fmt.Printf("\t%s\n", rec)
		default:
			fmt.Println(rec)
		}
	}

	return nil
}

// Cat prints a file at a given path to stdout
func (a *APFS) Cat(path string) error {
	var fsRecords types.FSRecords
	var decmpfsHdr *types.DecmpfsDiskHeader

	sr := a.r

	files, err := a.find(path)
	if err != nil {
		return fmt.Errorf("failed to find %s: %v", path, err)
	}

	for _, rec := range files {

		fsRecords, err = a.fsOMapBtree.GetFSRecordsForOid(sr, a.FSRootBtree, types.OidT(rec.Val.(types.JDrecVal).FileID), types.XidT(^uint64(0)))
		if err != nil {
			return fmt.Errorf("failed to get fs records for %s: %v", path, err)
		}

		var symlink string
		var fileName string
		// var uncompressedSize uint64
		var totalBytesWritten uint64
		var fexts []types.FileExtent

		compressed := false

		for _, rec := range fsRecords {
			switch rec.Hdr.GetType() {
			case types.APFS_TYPE_INODE:
				if rec.Val.(types.JInodeVal).InternalFlags&types.INODE_HAS_UNCOMPRESSED_SIZE != 0 {
					compressed = true
				}
				for _, xf := range rec.Val.(types.JInodeVal).Xfields {
					switch xf.XType {
					case types.INO_EXT_TYPE_NAME:
						fileName = xf.Field.(string)
					case types.INO_EXT_TYPE_DSTREAM:
						totalBytesWritten = xf.Field.(types.JDstreamT).TotalBytesWritten
					}
				}
			case types.APFS_TYPE_FILE_EXTENT:
				fexts = append(fexts, types.FileExtent{
					Address: rec.Key.(types.JFileExtentKeyT).LogicalAddr,
					Block:   rec.Val.(types.JFileExtentValT).PhysBlockNum,
					Length:  rec.Val.(types.JFileExtentValT).Length(),
				})
			case types.APFS_TYPE_XATTR:
				switch rec.Key.(types.JXattrKeyT).Name {
				case types.XATTR_RESOURCEFORK_EA_NAME:
					if rec.Val.(types.JXattrValT).Flags.DataEmbedded() {
						binary.Read(bytes.NewReader(rec.Val.(types.JXattrValT).Data.([]byte)), binary.LittleEndian, &decmpfsHdr)
					} else if rec.Val.(types.JXattrValT).Flags.DataStream() {
						xattrOid := types.OidT(rec.Val.(types.JXattrValT).Data.(types.JXattrDstreamT).XattrObjID)
						fsRecords, err = a.fsOMapBtree.GetFSRecordsForOid(sr, a.FSRootBtree, xattrOid, types.XidT(^uint64(0)))
						if err != nil {
							return fmt.Errorf("failed to get fs records for oid %#x: %v", xattrOid, err)
						}
						for _, rec := range fsRecords {
							switch rec.Hdr.GetType() {
							case types.APFS_TYPE_FILE_EXTENT:
								fexts = append(fexts, types.FileExtent{
									Address: rec.Key.(types.JFileExtentKeyT).LogicalAddr,
									Block:   rec.Val.(types.JFileExtentValT).PhysBlockNum,
									Length:  rec.Val.(types.JFileExtentValT).Length(),
								})
							}
						}
					}
				case types.XATTR_DECMPFS_EA_NAME:
					decmpfsHdr, err = types.GetDecmpfsHeader(rec)
					if err != nil {
						return fmt.Errorf("failed to get decmpfs header: %v", err)
					}
				case types.XATTR_SYMLINK_EA_NAME:
					symlink = string(rec.Val.(types.JXattrValT).Data.([]byte)[:])
					fmt.Println(symlink)
				}
			}
		}

		if compressed {
			w := bufio.NewWriter(os.Stdout)
			if err := decmpfsHdr.DecompressFile(a.r, a.geo, w, fexts, true); err != nil {
				return fmt.Errorf("failed to decompress %s: %v", fileName, err)
			}
			w.Flush()
		} else {
			for _, fext := range fexts {
				off, err := a.geo.BlockOffset(fext.Block)
				if err != nil {
					return fmt.Errorf("failed to compute file extent offset at block %#x: %v", fext.Block, err)
				}
				if err := a.dev.ReadFile(bufio.NewWriter(os.Stdout), off, int64(totalBytesWritten)); err != nil {
					return fmt.Errorf("failed to write file data from device: %v", err)
				}
			}
		}
	}

	return nil
}

// List lists files at a given path
func (a *APFS) List(path string) error {

	sr := a.r

	fsRecords, err := a.fsOMapBtree.GetFSRecordsForOid(sr, a.FSRootBtree, types.OidT(types.FSROOT_OID), types.XidT(^uint64(0)))
	if err != nil {
		return fmt.Errorf("failed to get FS records for FSROOT_OID: %v", err)
	}

	parts := strings.FieldsFunc(path, func(c rune) bool {
		return c == filepath.Separator
	})

	for idx, part := range parts {
		if len(part) > 0 {
			for _, rec := range fsRecords {
				switch rec.Hdr.GetType() {
				case types.APFS_TYPE_DIR_REC:
					if rec.Key.(types.JDrecHashedKeyT).Name == part {
						fsRecords, err = a.fsOMapBtree.GetFSRecordsForOid(sr, a.FSRootBtree, types.OidT(rec.Val.(types.JDrecVal).FileID), types.XidT(^uint64(0)))
						if err != nil {
							return fmt.Errorf("failed to get FS records for oid %#x: %v", types.OidT(rec.Val.(types.JDrecVal).FileID), err)
						}
						if idx == len(parts)-1 { // last part
							switch rec.Val.(types.JDrecVal).Flags {
							case types.DT_REG:
								var rFile types.RegFile
								for _, regRec := range fsRecords {
									switch regRec.Hdr.GetType() {
									case types.APFS_TYPE_INODE:
										rFile.Owner = regRec.Val.(types.JInodeVal).Owner
										rFile.Group = regRec.Val.(types.JInodeVal).Group
										rFile.Mode = regRec.Val.(types.JInodeVal).Mode
										rFile.CreateTime = regRec.Val.(types.JInodeVal).CreateTime

										for _, xf := range regRec.Val.(types.JInodeVal).Xfields {
											switch xf.XType {
											case types.INO_EXT_TYPE_NAME:
												rFile.Name = xf.Field.(string)
											case types.INO_EXT_TYPE_DSTREAM:
												rFile.Size = xf.Field.(types.JDstreamT).Size
											}
										}

										if regRec.Val.(types.JInodeVal).InternalFlags&types.INODE_HAS_UNCOMPRESSED_SIZE != 0 {
											rFile.Size = regRec.Val.(types.JInodeVal).UncompressedSize
										}
									}
								}
								fmt.Println(rFile)
								return nil
							case types.DT_LNK:
								var rFile types.RegFile
								for _, lnkRec := range fsRecords {
									switch lnkRec.Hdr.GetType() {
									case types.APFS_TYPE_INODE:
										for _, regRec := range fsRecords {
											switch regRec.Hdr.GetType() {
											case types.APFS_TYPE_INODE:
												rFile.Owner = regRec.Val.(types.JInodeVal).Owner
												rFile.Group = regRec.Val.(types.JInodeVal).Group
												rFile.Mode = regRec.Val.(types.JInodeVal).Mode
												rFile.CreateTime = regRec.Val.(types.JInodeVal).CreateTime

												for _, xf := range regRec.Val.(types.JInodeVal).Xfields {
													switch xf.XType {
													case types.INO_EXT_TYPE_NAME:
														rFile.Name = types.DirColor(xf.Field.(string)) // TODO: should this always be dir colored?
													case types.INO_EXT_TYPE_DSTREAM:
														rFile.Size = xf.Field.(types.JDstreamT).Size
													}
												}

												if regRec.Val.(types.JInodeVal).InternalFlags&types.INODE_HAS_UNCOMPRESSED_SIZE != 0 {
													rFile.Size = regRec.Val.(types.JInodeVal).UncompressedSize
												}
											}
										}
									case types.APFS_TYPE_XATTR:
										switch lnkRec.Key.(types.JXattrKeyT).Name {
										case types.XATTR_SYMLINK_EA_NAME:
											fmt.Printf("%s -> %s\n", rFile, string(lnkRec.Val.(types.JXattrValT).Data.([]byte)[:]))
										}
									}
								}
							}
						}
					}
					// default:
					// 	log.Warnf("found fs record type %s", rec.Hdr.GetType())
				}
			}
		}
	}

	fmt.Print(fsRecords)

	return nil
}

// Tree list contents of directories in a tree-like format. TODO: finish this
func (a *APFS) Tree(path string) error {

	sr := a.r

	fsRecords, err := a.fsOMapBtree.GetFSRecordsForOid(sr, a.FSRootBtree, types.OidT(types.FSROOT_OID), types.XidT(^uint64(0)))
	if err != nil {
		return err
	}

	fstree := types.NewFSTree("/")

	for part := range strings.SplitSeq(path, string(filepath.Separator)) {
		if len(part) > 0 {
			for _, rec := range fsRecords {
				switch rec.Hdr.GetType() {
				case types.APFS_TYPE_DIR_REC:
					if rec.Key.(types.JDrecHashedKeyT).Name == part {
						fsRecords, err = a.fsOMapBtree.GetFSRecordsForOid(sr, a.FSRootBtree, types.OidT(rec.Val.(types.JDrecVal).FileID), types.XidT(^uint64(0)))
						if err != nil {
							return err
						}
						fstree.AddTree(fsRecords.Tree())
					} else {
						fstree.Add(rec.Key.(types.JDrecHashedKeyT).Name)
					}
				default:
					log.Warnf("found fs record type %s", rec.Hdr.GetType())
				}
			}
		}
	}

	fmt.Println(fstree.Print())

	return nil
}

// Copy copies the contents of the src file to the dest file TODO: finish this
func (a *APFS) Copy(src, dest string) (err error) {
	entries, err := a.find(src)
	if err != nil {
		return fmt.Errorf("failed to find %s: %v", src, err)
	}
	// dest is always a directory: a file is extracted into it under its own name.
	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("failed to create destination %s: %w", dest, err)
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return fmt.Errorf("failed to open destination %s: %w", dest, err)
	}
	defer root.Close()

	return a.copyEntries(root, entries, ".")
}

// safeJoinPath accepts one record name. Filesystem operations must still use
// os.Root to prevent symlinks from escaping the extraction destination.
func safeJoinPath(dest, name string) (string, bool) {
	if !filepath.IsLocal(name) || name == "." || strings.ContainsAny(name, "/\\\x00") {
		return "", false
	}
	return filepath.Join(dest, name), true
}

// copyEntries extracts directory records into dest, a path relative to root.
// Subdirectories recurse by FileID, so each is listed with one lookup instead
// of re-resolving its full path from the volume root.
func (a *APFS) copyEntries(root *os.Root, entries []types.NodeEntry, dest string) error {
	for _, entry := range entries {
		if entry.Hdr.GetType() != types.APFS_TYPE_DIR_REC {
			continue
		}
		val := entry.Val.(types.JDrecVal)
		name := entry.Key.(types.JDrecHashedKeyT).Name
		if val.Flags != types.DT_DIR {
			if _, ok := safeJoinPath(dest, name); !ok {
				return fmt.Errorf("unsafe file record name %q in %s", name, dest)
			}
			if err := a.copyFile(root, entry, dest); err != nil {
				return err
			}
			continue
		}
		path, ok := safeJoinPath(dest, name)
		if !ok {
			return fmt.Errorf("unsafe directory record name %q in %s", name, dest)
		}
		if err := root.MkdirAll(path, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %v", path, err)
		}
		children, err := a.fsOMapBtree.GetFSRecordsForOid(a.r, a.FSRootBtree, types.OidT(val.FileID), types.XidT(^uint64(0)))
		if err != nil {
			return fmt.Errorf("failed to get fs records for oid %#x: %v", types.OidT(val.FileID), err)
		}
		if err := a.copyEntries(root, children, path); err != nil {
			return err
		}
	}
	return nil
}

func (a *APFS) copyFile(root *os.Root, rec types.NodeEntry, dest string) error {
	fsRecords, err := a.fsOMapBtree.GetFSRecordsForOid(a.r, a.FSRootBtree, types.OidT(rec.Val.(types.JDrecVal).FileID), types.XidT(^uint64(0)))
	if err != nil {
		return fmt.Errorf("failed to get fs records: %v", err)
	}

	var decmpfsHdr *types.DecmpfsDiskHeader
	var symlink string
	var fileName string
	var uncompressedSize uint64
	var totalBytesWritten uint64
	var fexts []types.FileExtent
	var foundInode bool

	compressed := false

	for _, rec := range fsRecords {
		switch rec.Hdr.GetType() {
		case types.APFS_TYPE_INODE:
			foundInode = true
			if rec.Val.(types.JInodeVal).InternalFlags&types.INODE_HAS_UNCOMPRESSED_SIZE != 0 {
				compressed = true
				uncompressedSize = rec.Val.(types.JInodeVal).UncompressedSize
			}
			for _, xf := range rec.Val.(types.JInodeVal).Xfields {
				switch xf.XType {
				case types.INO_EXT_TYPE_NAME:
					fileName = xf.Field.(string)
				case types.INO_EXT_TYPE_DSTREAM:
					totalBytesWritten = xf.Field.(types.JDstreamT).TotalBytesWritten
				}
			}
		case types.APFS_TYPE_FILE_EXTENT:
			fexts = append(fexts, types.FileExtent{
				Address: rec.Key.(types.JFileExtentKeyT).LogicalAddr,
				Block:   rec.Val.(types.JFileExtentValT).PhysBlockNum,
				Length:  rec.Val.(types.JFileExtentValT).Length(),
			})
		case types.APFS_TYPE_XATTR:
			switch rec.Key.(types.JXattrKeyT).Name {
			case types.XATTR_RESOURCEFORK_EA_NAME:
				if rec.Val.(types.JXattrValT).Flags.DataEmbedded() {
					decmpfsHdr = &types.DecmpfsDiskHeader{}
					if err := binary.Read(bytes.NewReader(rec.Val.(types.JXattrValT).Data.([]byte)), binary.LittleEndian, decmpfsHdr); err != nil {
						return fmt.Errorf("failed to read embedded decmpfs header: %v", err)
					}
				} else if rec.Val.(types.JXattrValT).Flags.DataStream() {
					xattrOid := types.OidT(rec.Val.(types.JXattrValT).Data.(types.JXattrDstreamT).XattrObjID)
					xattrRecords, err := a.fsOMapBtree.GetFSRecordsForOid(a.r, a.FSRootBtree, xattrOid, types.XidT(^uint64(0)))
					if err != nil {
						return fmt.Errorf("failed to get fs records for oid %#x: %v", xattrOid, err)
					}
					for _, xrec := range xattrRecords {
						switch xrec.Hdr.GetType() {
						case types.APFS_TYPE_FILE_EXTENT:
							fexts = append(fexts, types.FileExtent{
								Address: xrec.Key.(types.JFileExtentKeyT).LogicalAddr,
								Block:   xrec.Val.(types.JFileExtentValT).PhysBlockNum,
								Length:  xrec.Val.(types.JFileExtentValT).Length(),
							})
						}
					}
				}
			case types.XATTR_DECMPFS_EA_NAME:
				decmpfsHdr, err = types.GetDecmpfsHeader(rec)
				if err != nil {
					return fmt.Errorf("failed to get decmpfs header: %v", err)
				}
			case types.XATTR_SYMLINK_EA_NAME:
				symlink = strings.TrimRight(string(rec.Val.(types.JXattrValT).Data.([]byte)[:]), "\x00")
			}
		}
	}

	if !foundInode {
		return fmt.Errorf("missing inode for file oid %#x", rec.Val.(types.JDrecVal).FileID)
	}
	if fileName == "" {
		return fmt.Errorf("missing inode name for file oid %#x", rec.Val.(types.JDrecVal).FileID)
	}

	outPath, ok := safeJoinPath(dest, fileName)
	if !ok {
		return fmt.Errorf("unsafe file record name %q in %s", fileName, dest)
	}
	if symlink != "" {
		if err := root.Symlink(symlink, outPath); err != nil {
			return fmt.Errorf("failed to create symlink %s -> %s: %v", outPath, symlink, err)
		}
		return nil
	}
	fo, err := root.Create(outPath)
	if err != nil {
		return fmt.Errorf("failed to create %s: %v", outPath, err)
	}
	defer fo.Close()

	expectedSize := totalBytesWritten
	if compressed && decmpfsHdr == nil {
		return fmt.Errorf("missing decmpfs header for compressed file %s", outPath)
	}
	w := bufio.NewWriter(fo)
	if compressed {
		if err := decmpfsHdr.DecompressFile(a.r, a.geo, w, fexts, false); err != nil {
			return fmt.Errorf("failed to decompress and write %s: %v", outPath, err)
		}
		expectedSize = uncompressedSize
	} else {
		remaining := int64(totalBytesWritten)
		for _, fext := range fexts {
			if remaining <= 0 {
				break
			}
			extentBytes := min(int64(fext.Length), remaining)
			off, err := a.geo.BlockOffset(fext.Block)
			if err != nil {
				return fmt.Errorf("failed to compute file extent offset at block %#x: %v", fext.Block, err)
			}
			if err := a.dev.ReadFile(w, off, extentBytes); err != nil {
				return fmt.Errorf("failed to write file data from device: %v", err)
			}
			remaining -= extentBytes
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("failed to flush %s: %w", outPath, err)
	}
	info, err := fo.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat %s: %w", outPath, err)
	}
	if info.Size() != int64(expectedSize) {
		return fmt.Errorf("%s: wrote %d bytes, expected %d", outPath, info.Size(), expectedSize)
	}
	if err := fo.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", outPath, err)
	}
	log.Debugf("Created %s", outPath)

	return nil
}

// find resolves path to its directory records. A file or symlink resolves to
// its own record; a directory resolves to the records of its children.
func (a *APFS) find(path string) ([]types.NodeEntry, error) {
	fsRecords, err := a.fsOMapBtree.GetFSRecordsForOid(a.r, a.FSRootBtree, types.OidT(types.FSROOT_OID), types.XidT(^uint64(0)))
	if err != nil {
		return nil, fmt.Errorf("failed to get fs records for FSROOT_OID: %v", err)
	}

	parts := strings.FieldsFunc(path, func(c rune) bool {
		return c == filepath.Separator
	})

	for idx, part := range parts {
		match, ok := findDirRec(fsRecords, part)
		if !ok {
			return nil, fmt.Errorf("did not find file %s", path)
		}
		val := match.Val.(types.JDrecVal)
		switch val.Flags {
		case types.DT_DIR:
			fsRecords, err = a.fsOMapBtree.GetFSRecordsForOid(a.r, a.FSRootBtree, types.OidT(val.FileID), types.XidT(^uint64(0)))
			if err != nil {
				return nil, fmt.Errorf("failed to get fs records for oid %#x: %v", types.OidT(val.FileID), err)
			}
		case types.DT_REG, types.DT_LNK:
			if idx < len(parts)-1 {
				return nil, fmt.Errorf("%s is not a directory", part)
			}
			return []types.NodeEntry{match}, nil
		default:
			return nil, fmt.Errorf("%s has unsupported type %s", part, val.Flags)
		}
	}

	var children []types.NodeEntry
	for _, rec := range fsRecords {
		if rec.Hdr.GetType() == types.APFS_TYPE_DIR_REC {
			children = append(children, rec)
		}
	}
	return children, nil
}

func findDirRec(records []types.NodeEntry, name string) (types.NodeEntry, bool) {
	for _, rec := range records {
		if rec.Hdr.GetType() != types.APFS_TYPE_DIR_REC {
			continue
		}
		if rec.Key.(types.JDrecHashedKeyT).Name == name {
			return rec, true
		}
	}
	return types.NodeEntry{}, false
}
