## Introduction

Currently Zarf does not handle pulling index sha's for container images correctly. An index sha is a sha256 sum which is generated from an image index json file. View the definition of an image index here: https://github.com/opencontainers/image-spec/blob/main/image-index.md. Below is the image index for the zarf agent at version v0.32.6. There are three distinct digests that have been created at this tag. There is the arm64 digest, the amd64 digest, and the index digest. The index digest value is sha256 of the index.json. If the index sha is used without any additional options during a docker pull or crane pull the tool will pull down the correct image for your architecture. If an image index sha is used in Zarf it will break on deploy.

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

We create an image index in Zarf but it's used to specify all the images in a Zarf package rather than all the images at a specific tag. The image index within the Zarf package differs from image indexes registries store at tags (like the agent example above) in that it stores entirely different images rather than different platforms of images. It has all the images in the Zarf package. Because of this we annotate each entry in the manifest list with the name of the image we pull. This is what the current generated image index for a Zarf package looks like with one image at an index sha. The actual digest is different between the annotated digest. The actual digest, the one in the digest field begins with e81. The digest in the annotation field begins with ob6. This is because Zarf intended to pull down ob6, but in reality pulled down e81. Zarf uses crane to pull down images, crane will pull down the image according to the architecture specified by the user (or default user arch). This is why there is a mismatch. This is a bug

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

Simply changing the annotation `"org.opencontainers.image.base.name"` to correctly reflect the image name is not an adequate solution. This is because a user may reference the image in their manifests or helm charts. We want the experience to be as seamless as possible, so requiring users to have to change the image in their manifests or chart is not ideal. Especially since it would be hard to know that the sha of the image changed.

In order for a user to be able to use the image index sha we have to use the exact image index used in the remote registry we are pulling from. Using an index we create ourselves will not be an adequate solution. While we can create an image index that is an equivalent json, we don't know the order of the keys or the white space that is in the index in the remote registry. If the index is not a byte for byte copy the sha256 will change and the image will be unpushable & unpullable. We will have to pull the index down on create, store it in the zarf package, and push it up so the bytes are exactly the same.

Pulling everything in an image index may bloat Zarf packages. Some image indexes, for example, nginx:latest have as many as 8 distinct platforms. A user of Zarf may be surprised if they think they are importing a single image, but in reality import 8 different images. One advantage to pulling in every image specified by the image index is that we get multi arch packages. This would provide an easy way to bring in both an arm and amd architecture and allow users to deploy Zarf packages to clusters with arm64 and amd64 nodes natively.

Ideas for implementation:

## break on create if index sha is used

This will likely be the band aid fix, while we sort this out. Long term we can do better

## Multi / Any Architecture Zarf package

Introduce .metadata.architecture value called multi / any. When you select that value if your tags or sha points to an index sha then we pull everything down. This way we don't bloat the package size of users who already use tags having indexes, and don't lead users who don't understand OCI to download more images than they believe they will.  

Flow would look like this
if multi arch:
- index.json is downloaded along with all requisite items specified
If not multi arch:
- If using an image tagged with an index sha we use the default crane functionality and pull image for the user specified architecture (current behavior)
- If using an index sha you get the following error message
```
This sha points to an index.json file. This will break in the airgap unless we pull in all the images specified by the index.json, which may needlessly increase package size.
If you are deploying to a multi-platform cluster specify `metadata.architecture: multi` and we will pull all the images specified by this sha.
If you are only deploying to one platform I.E. linux/amd64. change the sha to your desired platform below. (Note: you will also have to change the sha in your manifests / helm charts)

If you only want to include the image for a single platform choose the sha for the platform below
- image@amd64sha # linux/amd64
- image@arm64sha # linux/arm64
```

### Consequences

- It may be confusing to call a package any / multi architecture when it potentially is only two architectures or even just one. In this scenario there would be nothing stopping users from grabbing single arch images, but we don't want to stop that since deployments might be limited to a set of nodes which are single arch.
- Less likely for users to mess up and accidentally bloat their packages as they will receive an explicit fail and have to manually set their architecture in order to pull in all the entries index.json
- Not a breaking change as it is opt in.

## Handle index sha's keep other behavior the same

When an image index sha is used pull the index.json along with all the manifest entries and push everything up to the Zarf registry. If a index sha is not used, keep behavior the same.

Warn when this happens so users know why their packages are bigger

### Consequences

- Users will need to use SHAs to have multi-platform support
- Zarf will label the package as a amd64 or arm64 architecture and it will be at least partially lying
- Users who don't understand how OCI work might be confused as to why seemingly one image is taking up potentially 8x the amount of space they thought it would (not everyone reads warnings)
- Not a breaking change since we would only act on image indexes which are already broken.

Current questions
- Where do we store the image index, in the images folder? That will be the initial attempt
  - yeah the images folder
- What is the source of truth for images on deploy. Images in the zarf.yaml or images in the index.json
  - Both are the source of truth
- Some images have platforms like unknown/unknown. What does that mean, why are they there
  - Those are [image attestations](https://docs.docker.com/build/attestations/attestation-storage) we'll have to see if we have to bring those over too or if we can create an image manifest without actually having everything it should point to
- Do I have to use the API to send it or can I use crane.copy() If I use crane.copy I have to have a local registry though I think, which means I probably can't
  - Probably can't use crane.copy(), but we may be able to pull the index
