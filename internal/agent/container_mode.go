package agent

import (
	"fmt"
	"strconv"

	"github.com/deployBunker/bunker/internal/imagespec"
)

// containerModeRunArgs builds the restricted workload command. It is not wired
// into spawn until persistence, execution, and orphan recovery are implemented.
func containerModeRunArgs(id string, uid, gid int, portStart, portEnd, hostPort, containerPort uint32, fullWipe bool) ([]string, error) {
	if !validAgentID.MatchString(id) || uid <= 0 || gid <= 0 {
		return nil, fmt.Errorf("invalid container agent identity")
	}
	if portStart < 10000 || portEnd > 19999 || portStart > portEnd || hostPort < portStart || hostPort > portEnd || containerPort == 0 || containerPort > 65535 {
		return nil, fmt.Errorf("invalid container port mapping")
	}
	home := "/home/" + strconv.Itoa(uid)
	args := []string{"--host", "unix:///run/bunker/" + id + "/docker.sock", "run", "-d", "--name", agentContainerName(id), "--user", fmt.Sprintf("%d:%d", uid, gid), "-e", "HOME=" + home, "-w", home}
	if !fullWipe {
		args = append(args, "-v", "/home/bunker-"+id+":"+home)
	}
	args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", hostPort, containerPort), imagespec.DefaultBaseImage, "sleep", "infinity")
	return args, nil
}
