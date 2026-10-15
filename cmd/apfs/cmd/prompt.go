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
	"os"
	"path/filepath"
	"strings"

	"github.com/apex/log"
	"github.com/blacktop/go-apfs"
	"github.com/blacktop/go-apfs/pkg/disk/dmg"
	"github.com/c-bata/go-prompt"
	"github.com/spf13/cobra"
)

type promptContext struct {
	pwd string
	a   *apfs.APFS
}

var pctx *promptContext

func completer(d prompt.Document) []prompt.Suggest {
	s := []prompt.Suggest{
		{Text: "cat", Description: "cat(1) file"},
		{Text: "cd", Description: "Change directory"},
		{Text: "cp", Description: "Copy file"},
		{Text: "exit", Description: "Quit prompt"},
		{Text: "ls", Description: "List files"},
		{Text: "pwd", Description: "Print working directory name"},
		{Text: "volume", Description: "Select a volume (index, name, uuid or role)"},
		{Text: "volumes", Description: "List the container's volumes"},
	}
	return prompt.FilterHasPrefix(s, d.TextBeforeCursor(), true)
}

func Executor(s string) {
	s = strings.TrimSpace(s)

	if s == "" {
		return
	} else if s == "exit" {
		os.Exit(0)
		return
	}

	args := strings.Fields(s)

	var path string

	if len(args) >= 2 {
		if strings.HasPrefix(args[1], "/") {
			path = args[1]
		} else {
			path = filepath.Join(pctx.pwd, args[1])
		}
	}

	switch args[0] {
	case "volumes":
		printVolumes(os.Stdout, pctx.a)
	case "volume":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: volume <index|name|uuid|role> (prefix with index:/name:/uuid:/role: to force)")
			printVolumes(os.Stdout, pctx.a)
			return
		}
		if err := pctx.a.SelectVolume(strings.Join(args[1:], " ")); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err.Error())
			return
		}
		selected := pctx.a.SelectedVolume()
		fmt.Fprintf(os.Stdout, "selected volume %d: %s (%s, %s)\n", selected.Index, selected.Name, selected.UUID, selected.Role)
		pctx.pwd = "/"
	case "cd":
		if len(args) == 1 {
			pctx.pwd = "/"
		} else if len(args) >= 2 {
			pctx.pwd = path
		}
	case "pwd":
		fmt.Println(pctx.pwd)
	case "ls":
		if len(args) > 1 {
			if err := pctx.a.List(path); err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err.Error())
				return
			}
		} else {
			if err := pctx.a.List(pctx.pwd); err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err.Error())
				return
			}
		}
	case "cat":
		if len(args) < 2 {
			return
		}
		for i := 1; i < len(args); i++ {
			if strings.HasPrefix(args[i], "/") {
				path = args[i]
			} else {
				path = filepath.Join(pctx.pwd, args[i])
			}
			if err := pctx.a.Cat(path); err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err.Error())
				return
			}
		}
	case "cp":
		if len(args) < 1 {
			return
		}

		if len(args) >= 3 {
			if err := pctx.a.Copy(path, args[2]); err != nil {
				fmt.Fprintln(os.Stderr, "Error:"+err.Error())
				return
			}
		} else {
			cwd, err := os.Getwd()
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error:"+err.Error())
				return
			}
			if err := pctx.a.Copy(path, cwd); err != nil {
				fmt.Fprintln(os.Stderr, "Error:"+err.Error())
				return
			}
		}
	default:
		fmt.Fprintln(os.Stderr, "command not found: "+args[0])
	}
}

func PromptPrefix() (string, bool) {
	volume := "<no volume>"
	if v := pctx.a.SelectedVolume(); v != nil {
		volume = v.Name
		if volume == "" {
			volume = v.Role.String()
		}
	}
	return fmt.Sprintf("[%s] %s > ", volume, pctx.pwd), true
}

// promptCmd represents the prompt command
var promptCmd = &cobra.Command{
	Use:   "prompt <DMG>",
	Short: "🚧 prompt to interactively run multiple commands",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if Verbose {
			log.SetLevel(log.DebugLevel)
		}

		a, dev, err := openAPFS(cmd, args[0], &dmg.Config{DisableCache: true})
		if err != nil {
			return err
		}
		defer a.Close()
		defer dev.Close()

		pctx = &promptContext{
			pwd: "/",
			a:   a,
		}

		if a.SelectedVolume() == nil {
			// Multi-volume container without a --volume selector: show the
			// available identities instead of guessing or panicking.
			fmt.Fprintln(os.Stderr, "This container has multiple volumes; select one with: volume <index|name|uuid|role>")
			printVolumes(os.Stderr, a)
		}

		p := prompt.New(Executor, completer, prompt.OptionLivePrefix(PromptPrefix))

		p.Run()

		return nil
	},
}

func init() {
	rootCmd.AddCommand(promptCmd)
	addVolumeFlag(promptCmd)
}
