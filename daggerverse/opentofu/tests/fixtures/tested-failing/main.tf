terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

# Broken twice over, on purpose: the test beside it asserts a length this
# resource does not have, so `tofu test` fails, and the block below is
# unformatted, so `tofu fmt` fails too. A Ci.Check with both stages enabled has
# to report both.
resource "random_pet" "name" {
    length = 2
      separator   =   "-"
}
