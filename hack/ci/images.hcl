group "default" { targets = ["public", "private"] }
variable "CACHE_FROM" { default = "" }
variable "CACHE_TO" { default = "" }
target "common" {
  context = "."
  dockerfile = "hack/ci/Dockerfile"
  cache-from = CACHE_FROM != "" ? ["type=local,src=${CACHE_FROM}/public", "type=local,src=${CACHE_FROM}/private"] : []
}
target "public" {
  inherits = ["common"]
  target = "public"
  cache-to = CACHE_TO != "" ? ["type=local,dest=${CACHE_TO}/public,mode=max"] : []
  tags = ["ghcr.io/datum-cloud/dns-operator:latest"]
}
target "private" {
  inherits = ["common"]
  target = "private"
  tags = ["internal-dns-kubernetes:e2e"]
  cache-to = CACHE_TO != "" ? ["type=local,dest=${CACHE_TO}/private,mode=max"] : []
}
