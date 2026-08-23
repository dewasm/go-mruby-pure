MRuby::Lockfile.disable

MRuby::Build.new do |conf|
  conf.toolchain
  conf.build_mrbc_exec
  conf.disable_libmruby
end

wasm = MRuby::CrossBuild.new('wasm32-wasi') do |conf|
  tools = File.expand_path('../tools', __dir__)
  conf.cc.command = File.join(tools, 'mruby-cc.sh')
  conf.linker.command = File.join(tools, 'mruby-cc.sh')
  conf.archiver.command = File.join(tools, 'mruby-ar.sh')
  conf.archiver.archive_options = 'rcs "%{outfile}" %{objs}'

  conf.exts.executable = '.wasm'

  profile = ENV.fetch('PROFILE', 'word')
  boxing =
    case profile
    when 'word' then %w[MRB_INT32 MRB_WORD_BOXING]
    when 'noboxing32' then %w[MRB_INT32 MRB_NO_BOXING]
    when 'noboxing' then %w[MRB_INT64 MRB_NO_BOXING]
    else abort "build_config: PROFILE=#{profile} is not one of word, noboxing32, noboxing"
    end
  (boxing + %w[MRB_UTF8_STRING MRB_USE_DEBUG_HOOK]).each { |d| conf.cc.defines << d }

  %w[
    mruby-compiler mruby-sprintf mruby-math mruby-string-ext mruby-array-ext
    mruby-enum-ext mruby-hash-ext mruby-numeric-ext mruby-symbol-ext
    mruby-object-ext mruby-error mruby-metaprog mruby-pack mruby-random
    mruby-time mruby-exit mruby-bigint
  ].each { |name| conf.gem core: name }
  conf.gem File.expand_path('mruby-wasi-puts', __dir__)
end

shim_src = File.expand_path('src/shim.c', __dir__)
shim_obj = File.join(wasm.build_dir, 'shim.o')
file shim_obj => [shim_src, MRUBY_CONFIG, wasm.libmruby_static] do
  wasm.cc.run(shim_obj, shim_src, %w[MRB_USE_BIGINT], [File.join(wasm.build_dir, 'include')])
end
task :all => shim_obj
