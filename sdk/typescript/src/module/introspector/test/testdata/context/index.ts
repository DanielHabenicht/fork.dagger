import { Directory } from "../../../../../api/client.gen.js"
import { func, object, argument } from "../../../../decorators.js"

@object()
export class Context {
  @func()
  helloWorld(
    @argument({ defaultPath: "." })
    dir: Directory,
  ): string {
    return `hello ${name}`
  }

  @func()
  helloWorldIgnored(
    @argument({ defaultPath: ".", ignore: ["dir"] })
    dir: Directory,
  ): string {
    return `hello ${name}`
  }

  @func()
  helloWorldIncludeExclude(
    @argument({ defaultPath: ".", include: ["**/*.webp"], exclude: ["**/node_modules"] })
    dir: Directory,
  ): string {
    return `hello ${name}`
  }
}
