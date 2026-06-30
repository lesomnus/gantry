# go-app

My flavor of "Hello, World!" for Go app.

## Quick Start

```sh
# Replace all occurrences of "github.com/lesomnus/go-app" with your own module path.
$ ./scripts/init.sh github.com/your-name/your-app

# Build and test the app.
# Build results will be placed in the `/dist` directory.
$ docker buildx bake build test

# Load apps into the local Docker engine.
$ docker buildx bake app --load
$ docker run --rm ghcr.io/lesomnus/go-app:local greet
> |........| 19:08:03.037 ○ 000000 000000 use default config
> Hello, hypnos!
```
