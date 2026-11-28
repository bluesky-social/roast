# syntax=docker/dockerfile:1@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183
FROM golang:1.27.4@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/roast-broker ./cmd/roast-broker
RUN bash scripts/licenses.sh "linux/$(go env GOARCH)"

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/roast-broker /roast-broker
COPY --from=build /src/LICENSE.txt /src/LICENSE-APACHE.txt /src/LICENSE-MIT.txt /usr/share/licenses/roast/
COPY --from=build /src/THIRD_PARTY_LICENSES /usr/share/licenses/roast/third-party/
EXPOSE 8080
ENTRYPOINT ["/roast-broker"]
