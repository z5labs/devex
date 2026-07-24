terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  # Deliberately credential-free and unreachable: Validate initialises with
  # -backend=false, so it never tries to talk to this bucket.
  backend "s3" {
    bucket = "devex-opentofu-tests-does-not-exist"
    key    = "state/terraform.tfstate"
    region = "us-east-1"
  }
}

resource "random_pet" "name" {
  length = 2
}
