run "mints_a_nonce" {
  command = apply

  assert {
    condition     = output.nonce == "never"
    error_message = "devex-nonce=${output.nonce}"
  }
}
