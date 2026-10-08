# The README demo

`../assets/demo.gif` is rendered with [Remotion](https://www.remotion.dev) from `src/script.ts`, which
replays output captured from a real run (`anyship init`, `plan`, `apply`, `status`, `destroy` against a
kubernetes cluster). When a command's output changes, update the script and render again:

```console
$ npm install
$ npm run render      # writes ../assets/demo.gif
$ npm run preview     # Remotion Studio, to scrub through it
```
