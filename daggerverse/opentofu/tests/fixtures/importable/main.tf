terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

# random_integer is the one hermetic resource that supports import: its id is
# `result,min,max`, so an object to adopt can be named outright instead of
# having to exist somewhere first. Which is what makes the import assertion
# meaningful — the result is fixed, so a plan afterwards is empty only if the
# import genuinely recorded it.
resource "random_integer" "adopted" {
  min = 1
  max = 100
}

output "adopted" {
  value = random_integer.adopted.result
}
