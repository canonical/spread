package spread

import (
	"os/exec"
)

type QemuProvider = qemuProvider

func QemuProviderQemuCmd(q *QemuProvider, imgPath string, port int) *exec.Cmd {
	return q.qemuCmd(imgPath, port)
}
