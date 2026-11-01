mock_provider "random" {}

run "names_the_pet" {
  assert {
    condition     = random_pet.name.length == 3
    error_message = "devex-failing-assertion: the pet name has ${random_pet.name.length} words, not 3"
  }
}
