group "default" { targets = ["public", "private"] }
variable "CACHE_FROM" { default = "" }
variable "CACHE_TO" { default = "" }
target "common" {
  context = "."
  dockerfile = "hack/ci/Dockerfile"
}
target "build" {
  inherits = ["common"]
  target = "build"
  output = ["type=cacheonly"]
  cache-from = CACHE_FROM != "" ? ["type=local,src=${CACHE_FROM}/build"] : []
  cache-to = CACHE_TO != "" ? ["type=local,dest=${CACHE_TO}/build,mode=max"] : []
}
target "public" {
  inherits = ["common"]
  target = "public"
  cache-from = CACHE_FROM != "" ? ["type=local,src=${CACHE_FROM}/public"] : []
  cache-to = CACHE_TO != "" ? ["type=local,dest=${CACHE_TO}/public,mode=min"] : []
  tags = ["ghcr.io/datum-cloud/dns-operator:latest"]
}
target "private" {
  inherits = ["common"]
  target = "private"
  cache-from = CACHE_FROM != "" ? ["type=local,src=${CACHE_FROM}/private"] : []
  tags = ["internal-dns-kubernetes:e2e"]
  cache-to = CACHE_TO != "" ? ["type=local,dest=${CACHE_TO}/private,mode=min"] : []
}
