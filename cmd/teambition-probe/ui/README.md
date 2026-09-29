# Swagger UI 静态资源

这里的 Swagger UI 使用官方 `swagger-ui-dist` **5.33.0**，JS、CSS 及许可证均已置于本目录，因此诊断页面运行时不依赖 CDN 或外网。资源来自 [npm 官方发布包](https://www.npmjs.com/package/swagger-ui-dist/v/5.33.0)，使用 Apache-2.0 许可证（见 `LICENSE`）；打包后的第三方版权声明见 `swagger-ui-bundle.js.LICENSE.txt`。

来源包的 SHA-512（npm integrity）为：

```
sha512-wpdK+m6BU5yj6pmUdMskZVTSWYG4DLglAx3sIhylloY37i8O37IrH+YEpqdXNfpaTGxILRBFzUqLF2jKqbfI7A==
```

`index.html` 从同一服务加载 `/openapi.yaml`。文档只公开获取 token 与读取已有任务两项诊断操作；AppSecret 和 appToken 始终由服务端处理。
