#!/usr/bin/env python3
# Convert an AdaFace (cvlface) face-recognition model to CoreML for the
# "Find people" feature. This is a maintainer/provenance script — the
# resulting .mlpackage is hosted and downloaded at runtime (see
# internal/helpers/model.go); end users never run this.
#
# Source model: minchul/cvlface_* on HuggingFace (AdaFace, mk-minchul,
# code MIT-licensed). The default is the IR-101 / WebFace12M backbone we
# ship; pass a different repo id for IR-50 etc.
#
# The CoreML model takes a 112x112 RGB face image and outputs a 512-d
# embedding. We bake AdaFace's input normalization ((x/255 - 0.5)/0.5 =
# (x - 127.5)/127.5) into the image input via scale=1/127.5, bias=-1, so
# the Swift helper can feed a plain RGB image (see build/faces-helper/main.swift).
#
# Usage (one-time, needs network):
#   uv venv --python 3.12 .venv
#   uv pip install --python .venv/bin/python torch coremltools transformers \
#       huggingface_hub safetensors omegaconf pyyaml fvcore torchvision
#   .venv/bin/python convert.py minchul/cvlface_adaface_ir101_webface12m AdaFace_IR101.mlpackage
#   ditto -c -k --keepParent AdaFace_IR101.mlpackage AdaFace_IR101.mlpackage.zip   # or: zip -r -X
#   shasum -a 256 AdaFace_IR101.mlpackage.zip   # -> goes into model.go's AdaFaceModel.SHA256
#
# coremltools (>=8) supports python<=3.12; torch 2.x. Newer combos may warn.

import sys, os, importlib.util
import torch
import coremltools as ct
from huggingface_hub import snapshot_download

REPO = sys.argv[1] if len(sys.argv) > 1 else "minchul/cvlface_adaface_ir101_webface12m"
OUT = sys.argv[2] if len(sys.argv) > 2 else "AdaFace.mlpackage"

snap = snapshot_download(REPO)
print("snapshot:", snap, flush=True)

# cvlface's wrapper.py uses relative paths (pretrained_model/..., a local
# `models` package, yaml configs), so run from inside the snapshot.
os.chdir(snap)
sys.path.insert(0, snap)
spec = importlib.util.spec_from_file_location("cvlwrap", os.path.join(snap, "wrapper.py"))
wrap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wrap)

model = wrap.CVLFaceRecognitionModel(wrap.ModelConfig()).eval()
print("model built", flush=True)


def pick(o):
    if isinstance(o, torch.Tensor):
        return o
    if isinstance(o, (list, tuple)):
        return pick(o[0])
    if isinstance(o, dict):
        for k in ("embedding", "feature", "features", "x"):
            if k in o:
                return o[k]
        return pick(list(o.values())[0])
    if hasattr(o, "embedding"):
        return o.embedding
    raise SystemExit(f"no embedding tensor in {type(o)}")


class Wrap(torch.nn.Module):
    def __init__(self, m):
        super().__init__()
        self.m = m

    def forward(self, x):
        return pick(self.m(x))


dummy = torch.randn(1, 3, 112, 112)
with torch.no_grad():
    print("embedding shape:", tuple(pick(model(dummy)).shape), flush=True)
    traced = torch.jit.trace(Wrap(model).eval(), dummy)

ml = ct.convert(
    traced,
    inputs=[ct.ImageType(name="face_image", shape=(1, 3, 112, 112),
                         scale=1 / 127.5, bias=[-1.0, -1.0, -1.0],
                         color_layout=ct.colorlayout.RGB)],
    outputs=[ct.TensorType(name="embedding")],
    minimum_deployment_target=ct.target.macOS13,
    compute_precision=ct.precision.FLOAT16,
    convert_to="mlprogram",
)
ml.short_description = f"{REPO}: 512-d face recognition embedding. Input 112x112 RGB."
ml.license = "MIT"
ml.save(OUT)
print("SAVED:", OUT, flush=True)
