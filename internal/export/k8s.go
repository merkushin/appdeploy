package export

import (
	"bytes"
	"fmt"
)

func k8s(s Spec) map[string]string {
	c := s.Config
	var b bytes.Buffer
	header(&b, "Kubernetes", s)
	// A Secret template: fill the values in (or create it with kubectl) and never commit them.
	fmt.Fprintf(&b, "---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\ntype: Opaque\nstringData:\n", q(s.App))
	names := allEnvNames(s)
	if len(names) == 0 {
		b.WriteString("  {}\n")
	}
	for _, n := range names {
		fmt.Fprintf(&b, "  %s: \"\"\n", n)
	}
	for _, name := range sortedRoles(c) {
		r := c.Roles[name]
		replicas := r.ReplicasPerSrv * len(hosts(s, r))
		fmt.Fprintf(&b, "---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %s\n  labels: {app: %s, role: %s}\nspec:\n  replicas: %d\n  selector:\n    matchLabels: {app: %s, role: %s}\n  template:\n    metadata:\n      labels: {app: %s, role: %s}\n    spec:\n      containers:\n        - name: %s\n          image: %s\n          envFrom:\n            - secretRef:\n                name: %s\n",
			q(s.App+"-"+name), q(s.App), q(name), replicas, q(s.App), q(name), q(s.App), q(name), q(name), q(image(s)), q(s.App))
		if r.Cmd != "" {
			b.WriteString("          args:\n")
			for _, a := range args(r.Cmd) {
				fmt.Fprintf(&b, "            - %s\n", q(a))
			}
		}
		if name == "web" {
			fmt.Fprintf(&b, "          env:\n            - {name: PORT, value: %q}\n          ports:\n            - containerPort: %d\n          readinessProbe:\n            httpGet: {path: %s, port: %d}\n            periodSeconds: 5\n            timeoutSeconds: %d\n",
				fmt.Sprint(webPort(c)), webPort(c), q(r.Healthcheck.Path), webPort(c), int(r.Healthcheck.Timeout.D().Seconds()))
		}
		fmt.Fprintf(&b, "          resources:\n            limits: {memory: %s, cpu: %q}\n", q(mem(r)+"i"), fmt.Sprint(r.CPUs))
	}
	if c.Roles["web"] != nil {
		fmt.Fprintf(&b, "---\napiVersion: v1\nkind: Service\nmetadata:\n  name: %s\nspec:\n  selector: {app: %s, role: web}\n  ports:\n    - port: 80\n      targetPort: %d\n", q(s.App), q(s.App), webPort(c))
		if len(s.Domains) > 0 {
			fmt.Fprintf(&b, "---\napiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata:\n  name: %s\nspec:\n  rules:\n", q(s.App))
			for _, d := range s.Domains {
				fmt.Fprintf(&b, "    - host: %s\n      http:\n        paths:\n          - path: /\n            pathType: Prefix\n            backend:\n              service:\n                name: %s\n                port: {number: 80}\n", q(d), q(s.App))
			}
		}
	}
	if c.Services.Postgres || c.Services.Redis {
		b.WriteString("# NOTE: this app used appdeploy's Postgres/Redis. Run them in the cluster (an operator or a\n# managed service) and put their URLs in the Secret above (DATABASE_URL / REDIS_URL).\n")
	}
	return map[string]string{"k8s.yml": b.String()}
}
