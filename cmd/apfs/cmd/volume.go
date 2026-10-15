/*
Copyright © 2020-2023 blacktop

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/
package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/apex/log"
	"github.com/spf13/cobra"

	"github.com/blacktop/go-apfs"
	"github.com/blacktop/go-apfs/pkg/disk"
	"github.com/blacktop/go-apfs/pkg/disk/dmg"
)

const volumeFlagHelp = `volume selector (index, name, uuid or role; prefix with index:/name:/uuid:/role: to force)`

// addVolumeFlag registers the common -v/--volume selector on a command.
func addVolumeFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("volume", "v", "", volumeFlagHelp)
}

// volumeSelector reads the registered --volume flag, if any.
func volumeSelector(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	sel, err := cmd.Flags().GetString("volume")
	if err != nil {
		return ""
	}
	return sel
}

// openAPFS opens the DMG at dmgPath, parses the APFS container and applies
// the command's --volume selector. The returned device must be closed by the
// caller. When no selector is given and the container has several volumes,
// the APFS object opens without an active volume and the first file-system
// operation returns the list of available identities.
func openAPFS(cmd *cobra.Command, dmgPath string, cfg *dmg.Config) (*apfs.APFS, disk.Device, error) {
	if cfg == nil {
		cfg = &dmg.Config{} // dmg.Open dereferences the config
	}
	dev, err := dmg.Open(filepath.Clean(dmgPath), cfg)
	if err != nil {
		return nil, nil, err
	}
	a, err := apfs.NewAPFS(dev)
	if err != nil {
		dev.Close()
		return nil, nil, err
	}
	if sel := volumeSelector(cmd); sel != "" {
		if err := a.SelectVolume(sel); err != nil {
			a.Close()
			dev.Close()
			return nil, nil, err
		}
	}
	return a, dev, nil
}

// printVolumes writes the stable volume table to w.
func printVolumes(w io.Writer, a *apfs.APFS) {
	fmt.Fprint(w, apfs.FormatVolumes(a.Volumes()))
}

// volumesCmd represents the volumes command
var volumesCmd = &cobra.Command{
	Use:   "volumes <DMG>",
	Short: "List the APFS volumes in a container (index, name, UUID, role)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if Verbose {
			log.SetLevel(log.DebugLevel)
		}

		a, dev, err := openAPFS(cmd, args[0], nil)
		if err != nil {
			return err
		}
		defer a.Close()
		defer dev.Close()

		printVolumes(os.Stdout, a)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(volumesCmd)
}
