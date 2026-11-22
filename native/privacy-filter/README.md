# Privacy Filter helper

`build.sh` builds the pinned privacy-filter.cpp and ggml sources with the
patches in this directory. Both macOS targets include Metal; the x86 base
helper remains CPU-only for machines without AVX2.

On Intel Metal, keep SIMD reduction support separate from SIMD matrix
support. The tested F16/Q8_0 path uses one 32-thread group when the pipeline
reports a wider execution width. Discrete Metal runs its graph in order and
uses owned staging buffers for arbitrary host pointers. A backend without
Flash Attention uses the existing explicit attention graph instead,
including when `PF_BANDED` was requested. `PF_NOFLASH` also disables the
flash-based banded path.

These compatibility paths have been tested on an AMD RX 570. They do not
establish support for every Intel or AMD GPU, or for other GGUF
quantizations. Short inputs benefit from GPU execution; the conservative
explicit-attention path can be slower than CPU on long inputs. The Intel
Metal warm-up is shorter to fit magpie's 40-second GPU load deadline, so a
first long request may still compile additional kernels.

## Verification

The normal build has no diagnostic tools. CI includes them in non-release
Intel artifacts only. For an Intel diagnostic package built locally:

```sh
PF_DIAGNOSTICS=1 native/privacy-filter/build.sh darwin-amd64 /tmp/pf-serve-darwin-amd64.zip
```

This adds `test-backend-ops` and `pf-cli` to the zip. CI cross-compiles the
Intel package; it does not run its GPU kernels on an Intel GPU. Run the
checks below on the target machine, using the extracted programs and an
existing local model. The checks never download a model or touch magpie's
configuration.

```sh
python3 -I native/privacy-filter/verify_test.py
/path/to/test-backend-ops test -b MTL0 -o NORM,RMS_NORM
/path/to/test-backend-ops test -b MTL0 -o SOFT_MAX
/path/to/test-backend-ops test -b MTL0 -o MUL_MAT -p 'type_a=(f16|q8_0),type_b=f32,m=16,n=(1|8|9|16),k=256,bs=\[1,1\],nr=\[1,1\],per=\[0,1,2,3\],k_v=0,o=1'
/path/to/test-backend-ops test -b MTL0 -o MUL_MAT_ID -p 'type_a=(f16|q8_0),type_b=f32,n_mats=(4|8),n_used=4,b=(0|1),m=512,n=(1|17|32|129),k=256'

python3 -I native/privacy-filter/verify.py run /path/to/pf-serve /path/to/model.gguf --device cpu --repeats 3 --output /tmp/pf-cpu.json
python3 -I native/privacy-filter/verify.py run /path/to/pf-serve /path/to/model.gguf --device gpu --repeats 5 --load-timeout 40 --output /tmp/pf-gpu.json
python3 -I native/privacy-filter/verify.py compare /tmp/pf-cpu.json /tmp/pf-gpu.json
```

The protocol check uses synthetic English, Chinese and code samples. It
rejects GPU-to-CPU fallback, compares every repeated result's byte offsets
and labels, and requires confidence scores to agree within 0.001. `--texts`
accepts a JSON array for longer synthetic cases, including inputs that cross
the model's window. Use new output paths for each run so earlier evidence is
not overwritten.

`pf-cli --dump-taps` can locate a divergent intermediate value. It changes
buffer lifetimes, so the final check must also pass through `pf-serve`
without tap capture.
