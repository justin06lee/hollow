package image

import (
	"fmt"
	"os"
	"sort"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

// WriteISO writes a small ISO 9660 image holding the given files at its root,
// with the given volume label. It is how a desk is handed its configuration
// and how the provisioning boot is handed cloud-init's user-data: a CD-ROM
// needs no network, no shared folder, and no guest driver beyond the one
// every OS has.
//
// Rock Ridge is on so that names survive as written. Plain ISO 9660 allows
// only upper-case letters, digits and underscores, and cloud-init looks for
// a file called exactly "user-data".
func WriteISO(path, label string, files map[string][]byte) error {
	var content int64
	for _, b := range files {
		content += int64(len(b)) + 2048
	}
	// Descriptors, path tables, the root directory and Rock Ridge entries
	// fit in well under a megabyte; the file is sparse, so the slack is free.
	size := content + 1<<20

	part := path + ".part"
	os.Remove(part)
	// ISO 9660 blocks are 2048 bytes, and the block size comes from the
	// sector size the disk is created with.
	d, err := diskfs.Create(part, size, diskfs.SectorSize(2048))
	if err != nil {
		return err
	}
	err = func() error {
		fs, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeISO9660, VolumeLabel: label})
		if err != nil {
			return err
		}
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			f, err := fs.OpenFile("/"+name, os.O_CREATE|os.O_RDWR)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if _, err := f.Write(files[name]); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		iso, ok := fs.(*iso9660.FileSystem)
		if !ok {
			return fmt.Errorf("unexpected filesystem type %T", fs)
		}
		return iso.Finalize(iso9660.FinalizeOptions{RockRidge: true, VolumeIdentifier: label})
	}()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, path)
}
