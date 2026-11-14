package deploy

import (
	"fmt"
	"strings"

	"appdeploy/internal/config"
	"appdeploy/internal/store"
)

// HandlePush decides whether a git push should start a deploy. The deploy
// branch comes from appdeploy.yml in the pushed commit, falling back to the
// branch the app was created with. It returns (nil, reason, nil) when the push
// is not for the deploy branch.
func (e *Engine) HandlePush(app, ref, sha string) (*store.Deploy, string, error) {
	a, err := e.st.GetApp(app)
	if err != nil {
		return nil, "", fmt.Errorf("app %q not found", app)
	}
	branch := a.Branch
	if data, _ := gitShow(e.repoDir(app), sha, config.FileName); data != nil {
		if cfg, err := config.ParseWith(data, config.Defaults{}); err == nil {
			branch = cfg.Branch
		}
	}
	if ref != "refs/heads/"+branch {
		return nil, fmt.Sprintf("%s is not the deploy branch (%s): not deploying", strings.TrimPrefix(ref, "refs/heads/"), branch), nil
	}
	d, err := e.Submit(Request{App: app, Ref: sha, Source: "push"})
	if err != nil {
		return nil, "", err
	}
	return &d, "", nil
}
