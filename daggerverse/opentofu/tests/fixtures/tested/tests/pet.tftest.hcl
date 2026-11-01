# The provider is mocked, so the test needs no credentials and touches nothing:
# tofu installs hashicorp/random for its schema and never calls it.
mock_provider "random" {
  mock_resource "random_pet" {
    defaults = {
      id = "devex-mocked-pet"
    }
  }
}

run "names_the_pet" {
  assert {
    condition     = random_pet.name.prefix == "devex"
    error_message = "the pet does not carry the default prefix"
  }

  assert {
    condition     = output.name == "devex-mocked-pet"
    error_message = "the name output does not come from the mocked pet"
  }
}
