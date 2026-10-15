package apfs

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/apex/log"
	"github.com/blacktop/go-apfs/types"
)

// maxXid selects the newest revision of every virtual object.
var maxXid = types.XidT(^uint64(0))

// Volume describes an APFS volume discovered in the container's current valid
// checkpoint. The Index is stable for the lifetime of the opened container and
// can be used with SelectVolumeByIndex.
type Volume struct {
	// Index is the ordinal of the volume in nx_fs_oid_list traversal order.
	Index int
	// Name is the volume name with trailing NUL padding removed.
	Name string
	// UUID is the volume's apfs_vol_uuid.
	UUID types.UUID
	// Role is the volume's role (System, Data, Preboot, Recovery, ...).
	Role types.VolRole
	// OID is the volume superblock's virtual object identifier.
	OID types.OidT
	// Mountable reports whether the volume superblock was read and identified
	// as a volume. When false, Issue explains why and selecting the volume
	// fails without disturbing any currently selected volume.
	Mountable bool
	// Issue describes why a volume is not Mountable.
	Issue string

	// paddr is the physical block address of the volume superblock.
	paddr uint64
	// notVolume records that the object at paddr is not OBJECT_TYPE_FS.
	notVolume bool
	// loadErr is the underlying discovery failure (checksum, parse error,
	// ...) retained so errors.Is keeps working when the volume is selected.
	loadErr error
}

// Sentinel errors allowing callers to disambiguate selection failures with
// errors.Is.
var (
	// ErrVolumeNotFound means no volume matches the supplied identifier.
	ErrVolumeNotFound = errors.New("volume not found")
	// ErrVolumeAmbiguous means more than one volume matches the identifier.
	ErrVolumeAmbiguous = errors.New("volume selector matches more than one volume")
	// ErrNotAVolume means the resolved object is not an APFS volume superblock.
	ErrNotAVolume = errors.New("object is not an APFS volume")
	// ErrVolumeNotMountable means the volume's superblock, object map, root
	// tree or node cache could not all be made ready.
	ErrVolumeNotMountable = errors.New("volume is not mountable")
	// ErrVolumeSelectionRequired means the container holds several volumes
	// and no selector was given.
	ErrVolumeSelectionRequired = errors.New("a volume must be selected before accessing the file system")
)

// VolumeNotFoundError is returned when an identifier does not resolve to a
// volume. It lists the available identities so the caller can refine the
// selector.
type VolumeNotFoundError struct {
	Kind     string // "index", "name", "uuid" or "role"
	Selector string
	Volumes  []Volume
}

func (e *VolumeNotFoundError) Error() string {
	return fmt.Sprintf("no volume found for %s %q\n%s", e.Kind, e.Selector, FormatVolumes(e.Volumes))
}

func (e *VolumeNotFoundError) Unwrap() error { return ErrVolumeNotFound }

// AmbiguousVolumeError is returned when a name or role resolves to more than
// one volume. It carries every candidate so the caller can disambiguate with
// an index or UUID.
type AmbiguousVolumeError struct {
	Kind     string // "name" or "role"
	Selector string
	Matches  []Volume
}

func (e *AmbiguousVolumeError) Error() string {
	return fmt.Sprintf("%s %q matches more than one volume; disambiguate with index or uuid\n%s",
		e.Kind, e.Selector, FormatVolumes(e.Matches))
}

func (e *AmbiguousVolumeError) Unwrap() error { return ErrVolumeAmbiguous }

// NotAVolumeError is returned when the object a selector resolves to exists
// but is not an APFS volume superblock.
type NotAVolumeError struct {
	OID    types.OidT
	Paddr  uint64
	Actual string
}

func (e *NotAVolumeError) Error() string {
	return fmt.Sprintf("object oid=%#x at paddr=%#x is %s, not a volume", e.OID, e.Paddr, e.Actual)
}

func (e *NotAVolumeError) Unwrap() error { return ErrNotAVolume }

// VolumeNotMountableError is returned when preparing a volume fails. The
// partially prepared candidate state is discarded and the previously
// selected volume (if any) stays active.
type VolumeNotMountableError struct {
	Volume Volume
	Err    error
}

func (e *VolumeNotMountableError) Error() string {
	return fmt.Sprintf("volume %q (index %d, uuid %s, role %s) is not mountable: %v",
		e.volumeLabel(), e.Volume.Index, e.Volume.UUID, e.Volume.Role, e.Err)
}

func (e *VolumeNotMountableError) volumeLabel() string {
	if e.Volume.Name != "" {
		return e.Volume.Name
	}
	return fmt.Sprintf("oid %#x", e.Volume.OID)
}

func (e *VolumeNotMountableError) Unwrap() []error { return []error{ErrVolumeNotMountable, e.Err} }

// NoVolumeSelectedError is returned from file-system operations on a
// multi-volume container where no volume has been selected yet.
type NoVolumeSelectedError struct {
	Volumes []Volume
}

func (e *NoVolumeSelectedError) Error() string {
	return fmt.Sprintf("container has %d volumes but none was selected; use SelectVolume or the --volume flag\n%s",
		len(e.Volumes), FormatVolumes(e.Volumes))
}

func (e *NoVolumeSelectedError) Unwrap() error { return ErrVolumeSelectionRequired }

// FormatVolumes renders one row per volume: stable index, NUL-trimmed name,
// UUID and role. Volumes that could not be identified keep their index and
// carry the failure reason in the role column.
func FormatVolumes(volumes []Volume) string {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "INDEX\tNAME\tUUID\tROLE")
	for _, v := range volumes {
		name := v.Name
		role := v.Role.String()
		if !v.Mountable {
			if name == "" {
				name = "<unreadable>"
			}
			role = fmt.Sprintf("unavailable (%s)", v.Issue)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", v.Index, name, v.UUID, role)
	}
	tw.Flush()
	return buf.String()
}

// Volumes returns the volumes present in the current valid checkpoint, in
// stable enumeration order. The returned slice is a copy.
func (a *APFS) Volumes() []Volume {
	out := make([]Volume, len(a.volumes))
	copy(out, a.volumes)
	return out
}

// SelectedVolume returns the volume that file-system operations currently
// target, or nil when none is selected.
func (a *APFS) SelectedVolume() *Volume {
	if a.selected == nil {
		return nil
	}
	v := *a.selected
	return &v
}

// SelectVolume selects the volume identified by selector. A bare selector is
// interpreted, in order, as: a decimal index, a UUID (with or without dashes),
// a role name, or a volume name. The kind can be forced with the prefixes
// "index:", "name:", "uuid:" or "role:".
//
// The selection only takes effect once the volume superblock, its object map,
// the file-system root tree and the node cache are all ready. On failure the
// candidate state is discarded and the previously selected volume, if any,
// remains active and listable.
func (a *APFS) SelectVolume(selector string) error {
	kind, value := parseVolumeSelector(selector)
	switch kind {
	case "index":
		idx, err := strconv.Atoi(value)
		if err != nil {
			return &VolumeNotFoundError{Kind: "index", Selector: selector, Volumes: a.volumes}
		}
		return a.SelectVolumeByIndex(idx)
	case "name":
		return a.SelectVolumeByName(value)
	case "uuid":
		return a.SelectVolumeByUUID(value)
	case "role":
		return a.SelectVolumeByRole(value)
	default:
		return &VolumeNotFoundError{Kind: kind, Selector: selector, Volumes: a.volumes}
	}
}

// SelectVolumeByIndex selects the volume shown at index by Volumes.
func (a *APFS) SelectVolumeByIndex(index int) error {
	for _, v := range a.volumes {
		if v.Index == index {
			return a.chooseVolume(v)
		}
	}
	return &VolumeNotFoundError{Kind: "index", Selector: strconv.Itoa(index), Volumes: a.volumes}
}

// SelectVolumeByName selects the single volume whose name matches exactly.
// Duplicate names produce an AmbiguousVolumeError.
func (a *APFS) SelectVolumeByName(name string) error {
	v, err := a.resolveUnique("name", name, func(v Volume) bool {
		return v.Mountable && v.Name == name
	})
	if err != nil {
		return err
	}
	return a.chooseVolume(*v)
}

// SelectVolumeByUUID selects the volume whose UUID matches. Dashes are
// optional and matching is case-insensitive.
func (a *APFS) SelectVolumeByUUID(uuid string) error {
	want, ok := parseUUID(uuid)
	if !ok {
		return &VolumeNotFoundError{Kind: "uuid", Selector: uuid, Volumes: a.volumes}
	}
	v, err := a.resolveUnique("uuid", uuid, func(v Volume) bool {
		return v.Mountable && v.UUID == want
	})
	if err != nil {
		return err
	}
	return a.chooseVolume(*v)
}

// SelectVolumeByRole selects the single volume with the given role name (see
// types.ParseVolRole). Multiple volumes sharing the role, which is common for
// role None, produce an AmbiguousVolumeError.
func (a *APFS) SelectVolumeByRole(role string) error {
	want, ok := types.ParseVolRole(role)
	if !ok {
		return &VolumeNotFoundError{Kind: "role", Selector: role, Volumes: a.volumes}
	}
	v, err := a.resolveUnique("role", role, func(v Volume) bool {
		return v.Mountable && v.Role == want
	})
	if err != nil {
		return err
	}
	return a.chooseVolume(*v)
}

// resolveUnique applies match to every known volume, returning the single
// candidate or a disambiguable error. Non-mountable volumes never match
// name/uuid/role selectors because their metadata is not trustworthy.
func (a *APFS) resolveUnique(kind, selector string, match func(Volume) bool) (*Volume, error) {
	var found []Volume
	for _, v := range a.volumes {
		if match(v) {
			found = append(found, v)
		}
	}
	switch len(found) {
	case 0:
		return nil, &VolumeNotFoundError{Kind: kind, Selector: selector, Volumes: a.volumes}
	case 1:
		return &found[0], nil
	default:
		return nil, &AmbiguousVolumeError{Kind: kind, Selector: selector, Matches: found}
	}
}

// chooseVolume rejects objects known not to be volumes and volumes already
// known to be broken before attempting a full activation.
func (a *APFS) chooseVolume(v Volume) error {
	if v.notVolume {
		if v.loadErr != nil {
			return v.loadErr
		}
		return &NotAVolumeError{OID: v.OID, Paddr: v.paddr, Actual: v.Issue}
	}
	if !v.Mountable {
		cause := v.loadErr
		if cause == nil {
			cause = errors.New(v.Issue)
		}
		return &VolumeNotMountableError{Volume: v, Err: cause}
	}
	return a.activateVolume(v)
}

// requireVolume gates file-system operations on a completed selection.
func (a *APFS) requireVolume() error {
	if a.fsOMapBtree == nil {
		return &NoVolumeSelectedError{Volumes: a.volumes}
	}
	return nil
}

// autoSelectVolume preserves the historical single-volume default: with one
// mountable volume it is activated immediately. With several, no volume is
// selected and callers must use SelectVolume; operations then fail with a
// NoVolumeSelectedError listing the available identities instead of guessing
// the first object-map record.
func (a *APFS) autoSelectVolume() error {
	var mountable []Volume
	for _, v := range a.volumes {
		if v.Mountable {
			mountable = append(mountable, v)
		}
	}
	switch len(mountable) {
	case 0:
		return fmt.Errorf("no mountable APFS volumes found in container:\n%s", FormatVolumes(a.volumes))
	case 1:
		log.Debugf("auto-selecting the only mountable volume %q (index %d)", mountable[0].Name, mountable[0].Index)
		return a.activateVolume(mountable[0])
	default:
		log.Debugf("container has %d mountable volumes; waiting for an explicit selection", len(mountable))
		return nil
	}
}

// discoverVolumes enumerates nx_fs_oid_list from the valid checkpoint
// container superblock, resolving every filesystem OID through the container
// object map. Only the volume superblocks are read at this stage so that a
// broken omap or root tree of one volume never blocks listing or selecting
// another.
func (a *APFS) discoverVolumes() error {
	root, err := a.containerOMapRoot()
	if err != nil {
		return err
	}

	a.volumes = make([]Volume, 0)
	for _, oid := range a.Valid.FsOids {
		if oid == types.OID_INVALID {
			continue
		}
		v := Volume{
			Index:     len(a.volumes),
			OID:       oid,
			Mountable: true,
		}
		entry, err := root.GetOMapEntry(a.r, oid, maxXid)
		if err != nil {
			v.Mountable = false
			v.loadErr = err
			v.Issue = fmt.Sprintf("container object map has no entry for oid %#x: %v", oid, err)
			a.volumes = append(a.volumes, v)
			continue
		}
		v.paddr = entry.Val.Paddr

		hdr, err := readObjectHeader(a.r, v.paddr)
		if err != nil {
			v.Mountable = false
			v.loadErr = err
			v.Issue = fmt.Sprintf("failed to read volume superblock at paddr %#x: %v", v.paddr, err)
			a.volumes = append(a.volumes, v)
			continue
		}
		if hdr.GetType() != types.OBJECT_TYPE_FS {
			v.Mountable = false
			v.notVolume = true
			v.loadErr = &NotAVolumeError{OID: oid, Paddr: v.paddr, Actual: hdr.GetType().String()}
			v.Issue = hdr.GetType().String()
			a.volumes = append(a.volumes, v)
			continue
		}

		obj, err := safeReadObj(a.r, v.paddr)
		if err != nil {
			v.Mountable = false
			v.loadErr = err
			v.Issue = fmt.Sprintf("failed to parse volume superblock at paddr %#x: %v", v.paddr, err)
			a.volumes = append(a.volumes, v)
			continue
		}
		sb, ok := obj.Body.(types.ApfsSuperblock)
		if !ok {
			v.Mountable = false
			v.loadErr = fmt.Errorf("decoded as %T, not a volume superblock", obj.Body)
			v.Issue = fmt.Sprintf("object at paddr %#x decoded as %T, not a volume superblock", v.paddr, obj.Body)
			a.volumes = append(a.volumes, v)
			continue
		}
		v.Name = trimVolumeName(sb.VolumeName[:])
		v.UUID = sb.VolumeUUID
		v.Role = sb.Role
		a.volumes = append(a.volumes, v)
	}
	return nil
}

// containerOMapRoot returns the root node of the container object map in the
// current valid checkpoint.
func (a *APFS) containerOMapRoot() (*types.BTreeNodePhys, error) {
	if a.Valid == nil || a.Valid.OMap == nil {
		return nil, fmt.Errorf("valid checkpoint has no container object map")
	}
	om, ok := a.Valid.OMap.Body.(types.OMap)
	if !ok {
		return nil, fmt.Errorf("valid checkpoint object map decoded as %T, not types.OMap", a.Valid.OMap.Body)
	}
	if om.Tree == nil {
		return nil, fmt.Errorf("valid checkpoint container object map has no B-tree")
	}
	tree, ok := om.Tree.Body.(types.BTreeNodePhys)
	if !ok {
		return nil, fmt.Errorf("container object map tree decoded as %T, not a B-tree node", om.Tree.Body)
	}
	return &tree, nil
}

// activateVolume prepares a volume and commits the selection atomically. All
// candidate state is local: the volume superblock is re-read from the device
// (never reused from enumeration), its omap tree gets a brand-new node cache,
// and the FS root is resolved fresh. Only after every step succeeds are the
// APFS fields swapped, so repeated selections, failures and volumes that reuse
// OIDs or physical addresses can never inherit the previous volume's root or
// cached nodes.
func (a *APFS) activateVolume(v Volume) error {
	cand, err := a.prepareVolume(v)
	if err != nil {
		return err
	}

	a.volume = cand.volumeObj
	a.Volume = cand.superblock
	a.fsOMapBtree = cand.omapRoot
	a.FSRootBtree = cand.fsRoot
	selected := v
	a.selected = &selected

	log.WithFields(log.Fields{
		"checksum": fmt.Sprintf("%#x", cand.volumeObj.Hdr.Checksum()),
		"type":     cand.volumeObj.Hdr.GetType(),
		"oid":      fmt.Sprintf("%#x", cand.volumeObj.Hdr.Oid),
		"xid":      fmt.Sprintf("%#x", cand.volumeObj.Hdr.Xid),
		"sub_type": cand.volumeObj.Hdr.GetSubType(),
		"flag":     cand.volumeObj.Hdr.GetFlag(),
	}).Debugf("Selected APFS Volume (%s)", v.Name)

	return nil
}

// volumeCandidate holds the fully prepared, not-yet-committed state of a
// selection attempt.
type volumeCandidate struct {
	volumeObj  *types.Obj
	superblock *types.ApfsSuperblock
	omapRoot   *types.BTreeNodePhys
	fsRoot     types.BTreeNodePhys
}

// prepareVolume runs every readiness step without touching the APFS object.
func (a *APFS) prepareVolume(v Volume) (*volumeCandidate, error) {
	hdr, err := readObjectHeader(a.r, v.paddr)
	if err != nil {
		return nil, &VolumeNotMountableError{Volume: v, Err: fmt.Errorf("failed to read volume superblock: %w", err)}
	}
	if hdr.GetType() != types.OBJECT_TYPE_FS {
		return nil, &NotAVolumeError{OID: v.OID, Paddr: v.paddr, Actual: hdr.GetType().String()}
	}

	volumeObj, err := safeReadObj(a.r, v.paddr)
	if err != nil {
		return nil, &VolumeNotMountableError{Volume: v, Err: fmt.Errorf("failed to parse volume superblock: %w", err)}
	}
	sb, ok := volumeObj.Body.(types.ApfsSuperblock)
	if !ok {
		return nil, &VolumeNotMountableError{Volume: v, Err: fmt.Errorf("decoded as %T, not a volume superblock", volumeObj.Body)}
	}
	if sb.OMap == nil {
		return nil, &VolumeNotMountableError{Volume: v, Err: errors.New("volume superblock has no object map")}
	}
	om, ok := sb.OMap.Body.(types.OMap)
	if !ok {
		return nil, &VolumeNotMountableError{Volume: v, Err: fmt.Errorf("volume object map decoded as %T", sb.OMap.Body)}
	}
	if om.Tree == nil {
		return nil, &VolumeNotMountableError{Volume: v, Err: errors.New("volume object map has no B-tree")}
	}
	omapTree, ok := om.Tree.Body.(types.BTreeNodePhys)
	if !ok {
		return nil, &VolumeNotMountableError{Volume: v, Err: fmt.Errorf("volume object map tree decoded as %T", om.Tree.Body)}
	}
	// A fresh root node carries a fresh cache: enabling it never mutates a
	// node still referenced by a previous selection.
	omapRoot := &omapTree
	if err := omapRoot.EnableNodeCache(fsNodeCacheSize); err != nil {
		return nil, &VolumeNotMountableError{Volume: v, Err: fmt.Errorf("failed to enable omap node cache: %w", err)}
	}

	log.Debugf("File System OMap Btree: %s", om.Tree)

	fsRootEntry, err := omapRoot.GetOMapEntry(a.r, sb.RootTreeOid, volumeObj.Hdr.Xid)
	if err != nil {
		return nil, &VolumeNotMountableError{Volume: v, Err: fmt.Errorf("failed to resolve root tree oid %#x: %w", sb.RootTreeOid, err)}
	}
	log.Debugf("File System Root Entry: %s", fsRootEntry)

	rootObj, err := safeReadObj(a.r, fsRootEntry.Val.Paddr)
	if err != nil {
		return nil, &VolumeNotMountableError{Volume: v, Err: fmt.Errorf("failed to read file-system root at paddr %#x: %w", fsRootEntry.Val.Paddr, err)}
	}
	fsRoot, ok := rootObj.Body.(types.BTreeNodePhys)
	if !ok {
		return nil, &VolumeNotMountableError{Volume: v, Err: fmt.Errorf("file-system root at paddr %#x decoded as %T, not a B-tree node", fsRootEntry.Val.Paddr, rootObj.Body)}
	}

	return &volumeCandidate{
		volumeObj:  volumeObj,
		superblock: &sb,
		omapRoot:   omapRoot,
		fsRoot:     fsRoot,
	}, nil
}

// readObjectHeader validates and decodes only the obj_phys_t at paddr. It
// lets callers identify non-volume objects without dispatching into ReadObj,
// which panics on object types it does not implement.
func readObjectHeader(r io.ReaderAt, paddr uint64) (types.ObjPhysT, error) {
	var hdr types.ObjPhysT
	block := make([]byte, types.BLOCK_SIZE)
	if _, err := r.ReadAt(block, int64(paddr*types.BLOCK_SIZE)); err != nil {
		return hdr, fmt.Errorf("failed to read object block at paddr %#x: %w", paddr, err)
	}
	if !types.VerifyChecksum(block) {
		return hdr, fmt.Errorf("object block at paddr %#x failed checksum validation: %w", paddr, types.ErrBadBlockChecksum)
	}
	if err := binary.Read(bytes.NewReader(block), binary.LittleEndian, &hdr); err != nil {
		return hdr, fmt.Errorf("failed to read obj_phys_t at paddr %#x: %w", paddr, err)
	}
	return hdr, nil
}

// safeReadObj calls types.ReadObj while converting its "not implemented"
// panics into ordinary errors. A corrupt container can point a filesystem OID
// anywhere, and enumeration must survive that.
func safeReadObj(r io.ReaderAt, paddr uint64) (obj *types.Obj, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			obj = nil
			err = fmt.Errorf("unsupported object at paddr %#x: %v", paddr, rec)
		}
	}()
	return types.ReadObj(r, paddr)
}

// trimVolumeName removes the trailing NUL padding of apfs_volname.
func trimVolumeName(raw []byte) string {
	return strings.TrimRight(string(raw), "\x00")
}

// parseUUID accepts canonical UUIDs and bare 32-hex-digit forms,
// case-insensitively.
func parseUUID(s string) (types.UUID, bool) {
	var u types.UUID
	clean := strings.ReplaceAll(strings.TrimSpace(s), "-", "")
	if len(clean) != 32 {
		return u, false
	}
	b, err := hex.DecodeString(clean)
	if err != nil {
		return u, false
	}
	copy(u[:], b)
	return u, true
}

// parseVolumeSelector splits a CLI-style selector into a kind and value.
// Explicit "index:/name:/uuid:/role:" prefixes win; otherwise a decimal
// integer is an index, a hex-with-dashes string is a UUID, a known role
// keyword is a role, and anything else is a name.
func parseVolumeSelector(selector string) (kind, value string) {
	s := strings.TrimSpace(selector)
	for _, k := range []string{"index", "name", "uuid", "role"} {
		if len(s) > len(k)+1 && strings.EqualFold(s[:len(k)], k) && s[len(k)] == ':' {
			return k, strings.TrimSpace(s[len(k)+1:])
		}
	}
	if isDecimalIndex(s) {
		return "index", s
	}
	if _, ok := parseUUID(s); ok {
		return "uuid", s
	}
	if _, ok := types.ParseVolRole(s); ok {
		return "role", s
	}
	return "name", s
}

func isDecimalIndex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
