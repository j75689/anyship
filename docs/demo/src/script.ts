// The demo replays real output, captured on 2026-10-08 from anyship v0.6.0
// against an OrbStack cluster, with the docker build progress shortened.

export type Line = {text: string; tone?: 'cmd' | 'ok' | 'warn' | 'info' | 'dim' | 'bold' | 'plain'};

export type Step = {
  command: string;
  lines: Line[];
  // How long the output takes to appear after the command, in seconds.
  work: number;
};

const ok = (text: string): Line => ({text, tone: 'ok'});
const warn = (text: string): Line => ({text, tone: 'warn'});
const info = (text: string): Line => ({text, tone: 'info'});
const dim = (text: string): Line => ({text, tone: 'dim'});
const bold = (text: string): Line => ({text, tone: 'bold'});
const cmd = (text: string): Line => ({text, tone: 'cmd'});
const plain = (text: string): Line => ({text});

export const steps: Step[] = [
  {
    command: 'anyship init',
    work: 0.6,
    lines: [
      bold('Detected:'),
      plain('  • go.mod found → Go module shop'),
      plain('  • main package at the module root'),
      plain('  • port 8080 from main.go'),
      plain(''),
      ok('✔ wrote anyship.yaml'),
      dim('Review it, commit it, then run `anyship plan --target <target>`.'),
    ],
  },
  {
    command: 'anyship plan -t kubernetes',
    work: 0.8,
    lines: [
      bold('Plan for kubernetes'),
      plain(''),
      plain('Findings:'),
      warn('  ! [web] Port 8080 is public, but without ingressClass no Ingress is made:'),
      warn('    shop-web is reachable inside the cluster only, at http://shop-web.default.svc:8080.'),
      dim('      → Set ingressClass to one of `kubectl get ingressclass` to route it.'),
      info('  i [web] No Dockerfile, so anyship generated one; review it in the generated files.'),
      plain(''),
      plain('Actions:'),
      plain('  $ image localhost:5000/shop/shop-web  docker buildx build --push, deployed by digest'),
      plain('  ↑ Deployment and Service shop-web  in namespace default, 1 replica(s)'),
      plain(''),
      plain('Generated files:'),
      plain('  ~ .anyship/kubernetes/web.Dockerfile'),
      plain('  ~ .anyship/kubernetes/manifests.yaml'),
      plain(''),
      ok('Plan is ready.'),
    ],
  },
  {
    command: 'anyship apply -t kubernetes --yes',
    work: 1.2,
    lines: [
      dim('checking namespace default'),
      dim('$ docker buildx build --platform linux/arm64 --push localhost:5000/shop/shop-web'),
      dim('#8 [build 4/4] RUN CGO_ENABLED=0 go build -o /out/app .'),
      dim('#13 pushing manifest for localhost:5000/shop/shop-web@sha256:7c02fd4a…'),
      dim('$ kubectl apply --prune -l anyship-project=shop'),
      plain('deployment.apps/shop-web created'),
      plain('service/shop-web created'),
      dim('$ kubectl rollout status deployment/shop-web'),
      plain('deployment "shop-web" successfully rolled out'),
      plain(''),
      bold('Checks on the target:'),
      info('  i Namespace default of orbstack is ready. Images are built for linux/arm64.'),
      plain(''),
      ok('✔ Deployed shop to namespace default of orbstack.'),
      ok('✔ web: http://shop-web.default.svc:8080 (inside the cluster)'),
    ],
  },
  {
    command: 'anyship status -t kubernetes',
    work: 0.6,
    lines: [
      bold('shop on kubernetes (orbstack/default)'),
      plain('  SERVICE  STATE    HEALTH  RUNNING  RESTARTS  SINCE     PORTS      URL  DETAIL'),
      plain('  web      running  -       1/1      0         just now  8080/http  -    1/1 available'),
      ok('All services are running.'),
    ],
  },
  {
    command: 'anyship destroy -t kubernetes --yes',
    work: 0.8,
    lines: [
      bold('Destroy shop on kubernetes:'),
      plain('  • Delete the Deployments, Services, Ingresses, CronJobs and autoscalers of shop-web.'),
      plain('  • Keep the namespace and the images in the registry.'),
      dim('$ kubectl delete deployments,services,ingresses,cronjobs,horizontalpodautoscalers -l anyship-project=shop'),
      plain('deployment.apps "shop-web" deleted'),
      plain('service "shop-web" deleted'),
      ok('✔ Removed shop from namespace default.'),
    ],
  },
];

export const cmdTone = cmd;
