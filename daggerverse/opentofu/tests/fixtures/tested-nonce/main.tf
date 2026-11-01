# A fresh UUID on every apply, and no provider. The test beside it fails on
# purpose with that UUID in its message, so two failures carrying the same one
# mean the second `tofu test` never ran.
output "nonce" {
  value = uuid()
}
