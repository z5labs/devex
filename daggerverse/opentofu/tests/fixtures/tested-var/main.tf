# No default and no provider: the variable has to arrive from whatever the
# caller bound to the Config, and nothing needs installing to prove it did.
variable "prefix" {
  type        = string
  description = "Bound by the test through WithVar, WithVarFile, WithSecretVar or WithEnvVariable."
}

output "prefix" {
  value = var.prefix
}
