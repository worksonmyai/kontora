class Kontora < Formula
  desc "Agent orchestration daemon — multi-stage pipelines with git worktree isolation"
  homepage "https://github.com/worksonmyai/kontora"
  license "Apache-2.0"

  url "https://github.com/worksonmyai/kontora.git",
      tag: "v0.19.0",
      revision: "62ab6e849bc192a035ac8b1500c577fd4b7cddc6",
      using: :git
  head "https://github.com/worksonmyai/kontora.git", branch: "main", using: :git

  depends_on "go" => :build

  def install
    system "make", "build"
    bin.install "kontora"
  end

  service do
    run [opt_bin/"kontora", "start"]
    keep_alive false
    log_path var/"log/kontora.log"
    error_log_path var/"log/kontora.log"
    environment_variables PATH: std_service_path_env
  end

  test do
    assert_match "kontora", shell_output("#{bin}/kontora --help 2>&1", 1)
  end
end
