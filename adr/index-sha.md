## Introduction

Currently Zarf does not handle pulling index sha's for container images correctly. An index sha is a sha256 sum which is generated from an image index json file. View the definition of an image index here: https://github.com/opencontainers/image-spec/blob/main/image-index.md. Below is the image index for the zarf agent at version v0.32.6. There are three potential digests that can be used for this tag digests to consider when importing an image into your project. You can use the arm64 digest, the amd64 digest, or the index digest. The index digest value is the value of the index.json, if this is used locally your pull tool of choice docker, crane, etc should pull down the correct image for your architecture. If an image index is used in Zarf it will break on deploy.

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:b3fabdc7d4ecd0f396016ef78da19002c39e3ace352ea0ae4baa2ce9d5958376",
      "size": 673,
      "platform": {
        "architecture": "arm64",
        "os": "linux"
      }
    },
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:454bf871b5d826b6a31ab14c983583ae9d9e30c2036606b500368c5b552d8fdf",
      "size": 673,
      "platform": {
        "architecture": "amd64",
        "os": "linux"
      }
    }
  ]
}
```

## Background

We use crane to pull images at the moment and we create an image index as well, but for all the images in the Zarf package rather than all the images at a specific tag. The image index within the images folder in the Zarf package differs from image indexes registries store at tags (like the one above). The image index in the zarf package stores more than one specific image at one specific tag. It has all the images in the Zarf package. Because of this we annotate each entry in the manifest list with the name of the image we pull. This is what the current generated image index for a Zarf package with one image at an index sha. The digests are different. The actual digest, the one in the digest field begins with e81. The digest in the annotation field begins with ob6. This is because Zarf intended to pull down ob6, but in reality pulled down e81. Zarf uses crane to pull down images, crane will pull down the image according to the architecture specified by the user (or default user arch). This is why there is a mismatch.

```json
  "manifests": [
    {
      "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
      "digest": "sha256:e81b1467b812019f8e8e81450728b084471806dc7e959b7beb9f39933c337e7d",
      "size": 739,
      "annotations": {
        "org.opencontainers.image.base.name": "docker.io/defenseunicorns/zarf-game:multi-tile-dark@sha256:0b694ca1c33afae97b7471488e07968599f1d2470c629f76af67145ca64428af"
      }
    }
  ]
```

Simply changing the annotation `"org.opencontainers.image.base.name"` to correctly reflect the image name is not an adequate solution. This is because a user may reference the image in their manifests or helm charts. We want the experience to be as seamless as possible, so requiring users to have to change the image in their manifests or chart is not ideal.

In order for a user to be able to use the image index sha we have to recreate the image index in the internal registry. This means Zarf will have to pull and push every image in the image index pointed to by the sha the user is bringing in to the zarf package and then the Zarf internal registry. Even when all the images are there the index sha will not work as the index still does not exist. Using an index we create ourselves will not be an adequate solution. While we can create an image index that is an equivalent json, we don't know the order of the keys or the whitespace that is in the index in the remote registry. We will have to pull that index down, store it in the zarf package, and push it up so the bytes are exactly the same.

This may bloat Zarf packages. Some image indexes, for example, nginx:latest have as many as 16 distinct platforms. A user of Zarf may be surprised if they think they are importing a single image, but in reality import 16 different images. We would have to warn around this. One advantage to pulling in multiple platforms is that we get multi arch clusters. This would provide an easy way to bring in both an arm and amd architecture for example.
