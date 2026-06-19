# Face-recognition model — provenance & conversion

The "Find people" feature clusters faces with a face-recognition embedding
model. WhatsKept does **not** bundle the model in the binary; it is fetched
on first use and verified by SHA-256 — see
`internal/helpers/model.go` (`AdaFaceModel`).

This folder documents exactly how that CoreML model was produced, so it's
reproducible and auditable.

## Shipped model

| | |
|---|---|
| Backbone | **AdaFace IR-101**, trained on **WebFace12M** |
| Source weights | [`minchul/cvlface_adaface_ir101_webface12m`](https://huggingface.co/minchul/cvlface_adaface_ir101_webface12m) (HuggingFace) |
| Upstream project | [mk-minchul/AdaFace](https://github.com/mk-minchul/AdaFace) |
| Format | CoreML `.mlpackage` (mlprogram, fp16), distributed as a `.zip` |
| Input | `face_image`: 112×112 **RGB**, aligned by the ArcFace 5-point template |
| Normalization | baked into the image input: `scale = 1/127.5`, `bias = -1` → `(x-127.5)/127.5` |
| Output | `embedding`: 512-d (the helper L2-normalizes it) |

The Swift helper (`build/faces-helper/main.swift`) detects + 5-point-aligns
each face to 112×112 RGB, runs this model via `VNCoreMLRequest`, and reads
the Float16 embedding.

## Licensing — read before redistributing

- **AdaFace code is MIT** (mk-minchul/AdaFace).
- The **weights are trained on WebFace12M** (a subset of WebFace260M). That
  dataset is distributed for **research use**; redistributing weights trained
  on it inside a broadly-distributed product is a gray area you should clear
  for your distribution. For a personal / open-source tool this is generally
  fine. If you need unambiguous commercial terms, swap to a model with
  permissively-licensed weights (e.g. **fal/AuraFace**) — it's the same
  112→512 interface, so only `convert.py`'s repo id and `model.go`'s
  `URL`/`SHA256`/`Bytes` change.

Include AdaFace's MIT license + attribution alongside the hosted artifact.

## Reproduce / re-convert

Needs network + Python ≤3.12 (coremltools constraint). See the header of
`convert.py` for the exact commands:

```sh
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python torch coremltools transformers \
    huggingface_hub safetensors omegaconf pyyaml fvcore torchvision
.venv/bin/python convert.py minchul/cvlface_adaface_ir101_webface12m AdaFace_IR101.mlpackage
ditto -c -k --keepParent AdaFace_IR101.mlpackage AdaFace_IR101.mlpackage.zip
shasum -a 256 AdaFace_IR101.mlpackage.zip   # -> AdaFaceModel.SHA256 in model.go
```

Then upload `AdaFace_IR101.mlpackage.zip` to a stable host (a GitHub release
on this repo, or a HuggingFace repo) and set `AdaFaceModel.URL` to it. The
SHA-256 + byte size in `model.go` must match the uploaded file.
