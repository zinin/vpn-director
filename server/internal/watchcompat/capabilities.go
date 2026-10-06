package watchcompat

import (
	"encoding/json"
	"io"
)

const ProtocolVersion = 1

type Capabilities struct {
	ProtocolVersion int    `json:"protocol_version"`
	WatchOwner      string `json:"watch_owner"`
}

func WriteCapabilities(w io.Writer) error {
	body, err := json.Marshal(Capabilities{ProtocolVersion: ProtocolVersion, WatchOwner: "watchd"})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	n, err := w.Write(body)
	if err == nil && n != len(body) {
		return io.ErrShortWrite
	}
	return err
}
