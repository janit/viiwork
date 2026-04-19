# Stage 1: Build llama.cpp with ROCm/gfx906
# Pinned to ROCm 6.2.4 -- last version with reliable gfx906 support
FROM rocm/dev-ubuntu-24.04:6.2.4-complete AS llama-build
RUN apt-get update && apt-get install -y cmake git
# Pin llama.cpp to a known-good release to prevent upstream breakage
ARG LLAMA_CPP_VERSION=b8660
RUN git clone --branch ${LLAMA_CPP_VERSION} --depth 1 \
    https://github.com/ggml-org/llama.cpp /llama.cpp
WORKDIR /llama.cpp
# gfx906 (Radeon VII) has no FP8 hardware; the HIP 6.2+ header defines
# __hip_fp8_e4m3 only for newer archs, causing a compile error.
RUN sed -i 's/#if HIP_VERSION >= 60200000/#if HIP_VERSION >= 99999999/' \
    ggml/src/ggml-cuda/vendors/hip.h
RUN cmake -B build -DGGML_HIP=ON -DAMDGPU_TARGETS=gfx906 \
    && cmake --build build --target llama-server llama-perplexity -j$(nproc)
# Smoke test: verify binary runs and can find HIP runtime
RUN ldd /llama.cpp/build/bin/llama-server    && /llama.cpp/build/bin/llama-server    --help > /dev/null
RUN ldd /llama.cpp/build/bin/llama-perplexity && /llama.cpp/build/bin/llama-perplexity --help > /dev/null

# Stage 2: Build viiwork
FROM golang:1.23.6 AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /viiwork ./cmd/viiwork

# Stage 3: Runtime
FROM rocm/dev-ubuntu-24.04:6.2.4-complete
RUN apt-get update && apt-get install -y --no-install-recommends ipmitool && rm -rf /var/lib/apt/lists/*
COPY --from=llama-build /llama.cpp/build/bin/llama-server    /usr/local/bin/
COPY --from=llama-build /llama.cpp/build/bin/llama-perplexity /usr/local/bin/
# Copy all shared libraries built by llama.cpp (includes libmtmd, libllama, libggml, etc.)
COPY --from=llama-build /llama.cpp/build /tmp/llama-build/
RUN find /tmp/llama-build -name '*.so*' -exec cp {} /usr/local/lib/ \; && rm -rf /tmp/llama-build && ldconfig
COPY --from=go-build /viiwork /usr/local/bin/
# Required: ROCm may not natively recognize gfx906 in all versions;
# this override forces gfx900-series compatibility
ENV HSA_OVERRIDE_GFX_VERSION=9.0.6
EXPOSE 8080
ENTRYPOINT ["viiwork"]
CMD ["--config", "/etc/viiwork/viiwork.yaml"]
