package(default_visibility = ["//visibility:public"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_library",
)

go_library(
    name = "go_default_library",
    srcs = ["ref.go"],
    importmap = "github.com/dotmesh-io/dotmesh/cmd/operator/vendor/k8s.io/client-go/tools/reference",
    importpath = "k8s.io/client-go/tools/reference",
    deps = [
        "//cmd/operator/vendor/k8s.io/api/core/v1:go_default_library",
        "//cmd/operator/vendor/k8s.io/apimachinery/pkg/api/meta:go_default_library",
        "//cmd/operator/vendor/k8s.io/apimachinery/pkg/apis/meta/v1:go_default_library",
        "//cmd/operator/vendor/k8s.io/apimachinery/pkg/runtime:go_default_library",
    ],
)

filegroup(
    name = "package-srcs",
    srcs = glob(["**"]),
    tags = ["automanaged"],
    visibility = ["//visibility:private"],
)

filegroup(
    name = "all-srcs",
    srcs = [":package-srcs"],
    tags = ["automanaged"],
)
