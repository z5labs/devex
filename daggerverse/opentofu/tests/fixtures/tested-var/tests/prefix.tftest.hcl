run "prefix_arrives" {
  assert {
    condition     = output.prefix == "from-bound-var"
    error_message = "devex-unbound-prefix: the prefix variable did not arrive"
  }
}
