package dockertest

import (
	_ "github.com/taskcluster/taskcluster-worker/engines/docker"
	_ "github.com/taskcluster/taskcluster-worker/plugins/artifacts"
	_ "github.com/taskcluster/taskcluster-worker/plugins/cache"
	_ "github.com/taskcluster/taskcluster-worker/plugins/env"
	_ "github.com/taskcluster/taskcluster-worker/plugins/livelog"
	_ "github.com/taskcluster/taskcluster-worker/plugins/maxruntime"
	_ "github.com/taskcluster/taskcluster-worker/plugins/reboot"
	_ "github.com/taskcluster/taskcluster-worker/plugins/stoponerror"
	_ "github.com/taskcluster/taskcluster-worker/plugins/success"
	_ "github.com/taskcluster/taskcluster-worker/plugins/watchdog"
)
