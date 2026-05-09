# random

A Dagger module for generating random values.

Each function bypasses Dagger's function cache (`+cache="never"`), so every
invocation returns a fresh value.

## Functions

- [UuidV4](#uuidv4) — random UUID v4
- [UuidV7](#uuidv7) — random UUID v7 (time-ordered)
- [Sha256](#sha256) — SHA-256 hash of N random bytes
- [Sha512](#sha512) — SHA-512 hash of N random bytes

### UuidV4

Generates a random UUID version 4 and returns it as a string.

CLI:

```sh
dagger -m github.com/z5labs/devex/daggerverse/random call uuid-v-4
# fdf09f71-206c-401b-a02a-c1095984af30
```

Go SDK:

```go
id, err := dag.Random().UUIDV4(ctx)
```

### UuidV7

Generates a random UUID version 7 (time-ordered) and returns it as a string.

CLI:

```sh
dagger -m github.com/z5labs/devex/daggerverse/random call uuid-v-7
# 019e0db1-e9b2-717f-93d2-76915dd707f1
```

Go SDK:

```go
id, err := dag.Random().UUIDV7(ctx)
```

### Sha256

Generates `n` random bytes and returns their SHA-256 hash as a hexadecimal
string. `n` defaults to 32.

CLI:

```sh
dagger -m github.com/z5labs/devex/daggerverse/random call sha-256
dagger -m github.com/z5labs/devex/daggerverse/random call sha-256 --n=64
```

Go SDK:

```go
h, err := dag.Random().Sha256(ctx)
h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 64})
```

### Sha512

Generates `n` random bytes and returns their SHA-512 hash as a hexadecimal
string. `n` defaults to 64.

CLI:

```sh
dagger -m github.com/z5labs/devex/daggerverse/random call sha-512
dagger -m github.com/z5labs/devex/daggerverse/random call sha-512 --n=128
```

Go SDK:

```go
h, err := dag.Random().Sha512(ctx)
h, err := dag.Random().Sha512(ctx, dagger.RandomSha512Opts{N: 128})
```
