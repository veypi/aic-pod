#!/usr/bin/env python3
"""
gen-project.py — 生成 IvecAI.xcodeproj

mbp 上没有 xcodegen / tuist，工程文件由本脚本产出（可重复执行，结果稳定）。
只做一件事：把 IvecAI/ 下的源码与资源写进一个单 target 的 iOS App 工程。

用法：
    python3 scripts/gen-project.py
    python3 scripts/gen-project.py --project-name IvecAI --bundle-id com.ivec.ai
"""

import argparse
import hashlib
import os
import sys
from pathlib import Path

FILE_TYPES = {
    ".swift": "sourcecode.swift",
    ".xcassets": "folder.assetcatalog",
    ".xcprivacy": "text.plist.privacy",
    ".plist": "text.plist.xml",
    ".entitlements": "text.plist.entitlements",
    ".md": "net.daringfireball.markdown",
    ".json": "text.json",
    ".html": "text.html",
    ".png": "image.png",
    ".sh": "text.script.sh",
    ".py": "text.script.python",
}

SOURCE_EXTS = {".swift"}
RESOURCE_NAMES = {"Assets.xcassets", "PrivacyInfo.xcprivacy"}


def uid(*parts: str) -> str:
    """稳定 UUID：同样的输入永远得到同样的标识（工程文件可重复生成）。"""
    digest = hashlib.sha1("::".join(parts).encode("utf-8")).hexdigest().upper()
    return digest[:24]


class Project:
    def __init__(self, root: Path, project_name: str, bundle_id: str, team_id: str,
                 deployment_target: str, marketing_version: str, build_version: str):
        self.root = root
        self.project_name = project_name
        self.bundle_id = bundle_id
        self.team_id = team_id
        self.deployment_target = deployment_target
        self.marketing_version = marketing_version
        self.build_version = build_version
        self.objects: list[str] = []
        self.file_refs: dict[str, str] = {}

    # ---------- 收集文件 ----------
    def collect(self) -> tuple[list[Path], list[Path]]:
        sources, resources = [], []
        base = self.root / "IvecAI"
        for path in sorted(base.rglob("*")):
            if path.name.startswith("."):
                continue
            if path.is_dir():
                # xcassets 是目录，但它是资源（会被 actool 编译成 Assets.car）。
                if path.suffix == ".xcassets":
                    resources.append(path)
                continue
            if path.suffix in SOURCE_EXTS:
                sources.append(path)
            elif path.name in RESOURCE_NAMES:
                resources.append(path)
        return sources, resources

    # ---------- pbxproj 片段 ----------
    def file_ref(self, path: Path) -> str:
        rel = path.relative_to(self.root)
        key = str(rel)
        if key in self.file_refs:
            return self.file_refs[key]
        ref = uid("fileref", key)
        self.file_refs[key] = ref
        kind = FILE_TYPES.get(path.suffix, "text")
        name = path.name
        self.objects.append(
            f'\t\t{ref} /* {name} */ = {{isa = PBXFileReference; '
            f'lastKnownFileType = {kind}; path = "{name}"; sourceTree = "<group>"; }};'
        )
        return ref

    def build_file(self, path: Path, phase: str) -> str:
        ref = self.file_ref(path)
        build = uid("buildfile", phase, str(path.relative_to(self.root)))
        self.objects.append(
            f'\t\t{build} /* {path.name} in {phase} */ = {{isa = PBXBuildFile; '
            f'fileRef = {ref} /* {path.name} */; }};'
        )
        return build

    def group_tree(self) -> tuple[str, str]:
        """建立与磁盘目录一致的 group 树，返回 (mainGroupId, appGroupId)。"""
        base = self.root / "IvecAI"
        groups: dict[str, str] = {}

        def group_for(rel_dir: Path) -> str:
            key = str(rel_dir)
            if key in groups:
                return groups[key]
            gid = uid("group", key)
            groups[key] = gid
            children: list[str] = []
            for child in sorted((self.root / rel_dir).iterdir()):
                if child.is_dir():
                    # xcassets 必须是一个资源引用（folder.assetcatalog）：当成普通
                    # group 的话 Xcode 不会编译出 Assets.car，图标与图片都进不了包。
                    if child.suffix == ".xcassets":
                        children.append(self.file_ref(child))
                    else:
                        children.append(group_for(child.relative_to(self.root)))
                elif child.is_file() and not child.name.startswith("."):
                    children.append(self.file_ref(child))
            path_attr = f'path = "{rel_dir.name}"; ' if rel_dir != Path(".") else ""
            self.objects.append(
                f'\t\t{gid} /* {rel_dir.name or "IvecAI"} */ = {{isa = PBXGroup; '
                f'children = ({", ".join(children)}); {path_attr}sourceTree = "<group>"; }};'
            )
            return gid

        app_group = group_for(base.relative_to(self.root))
        return app_group, app_group

    # ---------- 主生成 ----------
    def generate(self) -> str:
        sources, resources = self.collect()
        app_group, _ = self.group_tree()

        product_ref = uid("product", self.project_name)
        self.objects.append(
            f'\t\t{product_ref} /* {self.project_name}.app */ = {{isa = PBXFileReference; '
            f'explicitFileType = wrapper.application; includeInIndex = 0; '
            f'path = {self.project_name}.app; sourceTree = BUILT_PRODUCTS_DIR; }};'
        )

        main_group = uid("group", "main")
        self.objects.append(
            f'\t\t{main_group} = {{isa = PBXGroup; children = ({app_group}, {product_ref}); '
            f'sourceTree = "<group>"; }};'
        )

        source_builds = [self.build_file(p, "Sources") for p in sources]
        resource_builds = [self.build_file(p, "Resources") for p in resources]

        sources_phase = uid("phase", "sources")
        self.objects.append(
            f'\t\t{sources_phase} /* Sources */ = {{isa = PBXSourcesBuildPhase; '
            f'buildActionMask = 2147483647; files = ({", ".join(source_builds)}); '
            f'runOnlyForDeploymentPostprocessing = 0; }};'
        )
        resources_phase = uid("phase", "resources")
        self.objects.append(
            f'\t\t{resources_phase} /* Resources */ = {{isa = PBXResourcesBuildPhase; '
            f'buildActionMask = 2147483647; files = ({", ".join(resource_builds)}); '
            f'runOnlyForDeploymentPostprocessing = 0; }};'
        )
        frameworks_phase = uid("phase", "frameworks")
        self.objects.append(
            f'\t\t{frameworks_phase} /* Frameworks */ = {{isa = PBXFrameworksBuildPhase; '
            f'buildActionMask = 2147483647; files = (); runOnlyForDeploymentPostprocessing = 0; }};'
        )

        target_id = uid("target", self.project_name)
        target_config_list = uid("configlist", "target", self.project_name)
        project_config_list = uid("configlist", "project", self.project_name)

        target = (
            f'\t\t{target_id} /* {self.project_name} */ = {{\n'
            f'\t\t\tisa = PBXNativeTarget;\n'
            f'\t\t\tbuildConfigurationList = {target_config_list} /* Build configuration list for PBXNativeTarget "{self.project_name}" */;\n'
            f'\t\t\tbuildPhases = (\n'
            f'\t\t\t\t{sources_phase} /* Sources */,\n'
            f'\t\t\t\t{frameworks_phase} /* Frameworks */,\n'
            f'\t\t\t\t{resources_phase} /* Resources */,\n'
            f'\t\t\t);\n'
            f'\t\t\tbuildRules = (\n\t\t\t);\n'
            f'\t\t\tdependencies = (\n\t\t\t);\n'
            f'\t\t\tname = {self.project_name};\n'
            f'\t\t\tproductName = {self.project_name};\n'
            f'\t\t\tproductReference = {product_ref} /* {self.project_name}.app */;\n'
            f'\t\t\tproductType = "com.apple.product-type.application";\n'
            '\t\t};'
        )
        self.objects.append(target)

        project_id = uid("project", self.project_name)
        project = (
            f'\t\t{project_id} /* Project object */ = {{\n'
            f'\t\t\tisa = PBXProject;\n'
            f'\t\t\tattributes = {{\n'
            f'\t\t\t\tBuildIndependentTargetsInParallel = 1;\n'
            f'\t\t\t\tLastSwiftUpdateCheck = 1600;\n'
            f'\t\t\t\tLastUpgradeCheck = 1600;\n'
            f'\t\t\t\tTargetAttributes = {{\n'
            f'\t\t\t\t\t{target_id} = {{\n\t\t\t\t\t\t\tCreatedOnToolsVersion = 16.0;\n'
            f'\t\t\t\t\t}};\n'
            f'\t\t\t\t}};\n'
            f'\t\t\t}};\n'
            f'\t\t\tbuildConfigurationList = {project_config_list} /* Build configuration list for PBXProject "{self.project_name}" */;\n'
            f'\t\t\tcompatibilityVersion = "Xcode 15.0";\n'
            f'\t\t\tdevelopmentRegion = en;\n'
            f'\t\t\thasScannedForEncodings = 0;\n'
            f'\t\t\tknownRegions = (\n\t\t\t\ten,\n\t\t\t\tBase,\n\t\t\t\tzh_CN,\n\t\t\t);\n'
            f'\t\t\tmainGroup = {main_group};\n'
            f'\t\t\tproductRefGroup = {main_group} /* Products */;\n'
            f'\t\t\tprojectDirPath = "";\n'
            f'\t\t\tprojectRoot = "";\n'
            f'\t\t\ttargets = (\n\t\t\t\t{target_id} /* {self.project_name} */,\n\t\t\t\t);\n'
            '\t\t};'
        )
        self.objects.append(project)

        # ---- 构建设置 ----
        project_common = [
            ("ALWAYS_SEARCH_USER_PATHS", "NO"),
            ("CLANG_ANALYZER_NONNULL", "YES"),
            ("CLANG_ENABLE_MODULES", "YES"),
            ("CLANG_ENABLE_OBJC_ARC", "YES"),
            ("COPY_PHASE_STRIP", "NO"),
            ("ENABLE_STRICT_OBJC_MSGSEND", "YES"),
            ("ENABLE_USER_SCRIPT_SANDBOXING", "YES"),
            ("GCC_C_LANGUAGE_STANDARD", "gnu17"),
            ("IPHONEOS_DEPLOYMENT_TARGET", self.deployment_target),
            ("SDKROOT", "iphoneos"),
            ("SWIFT_VERSION", "5.0"),
        ]
        project_debug = project_common + [
            ("DEBUG_INFORMATION_FORMAT", "dwarf"),
            ("ENABLE_TESTABILITY", "YES"),
            ("GCC_OPTIMIZATION_LEVEL", "0"),
            ("GCC_PREPROCESSOR_DEFINITIONS", '(\n\t\t\t\t\t"DEBUG=1",\n\t\t\t\t\t"$(inherited)",\n\t\t\t\t)'),
            ("MTL_ENABLE_DEBUG_INFO", "INCLUDE_SOURCE"),
            ("ONLY_ACTIVE_ARCH", "YES"),
            ("SWIFT_ACTIVE_COMPILATION_CONDITIONS", '"DEBUG $(inherited)"'),
            ("SWIFT_OPTIMIZATION_LEVEL", '"-Onone"'),
        ]
        project_release = project_common + [
            ("DEBUG_INFORMATION_FORMAT", '"dwarf-with-dsym"'),
            ("ENABLE_NS_ASSERTIONS", "NO"),
            ("MTL_ENABLE_DEBUG_INFO", "NO"),
            ("SWIFT_COMPILATION_MODE", "wholemodule"),
            ("SWIFT_OPTIMIZATION_LEVEL", '"-O"'),
            ("VALIDATE_PRODUCT", "YES"),
        ]
        target_common = [
            ("ASSETCATALOG_COMPILER_APPICON_NAME", "AppIcon"),
            ("CODE_SIGN_STYLE", "Automatic"),
            ("CURRENT_PROJECT_VERSION", self.build_version),
            ("DEVELOPMENT_TEAM", self.team_id),
            ("ENABLE_PREVIEWS", "YES"),
            ("GENERATE_INFOPLIST_FILE", "NO"),
            ("INFOPLIST_FILE", "IvecAI/Resources/Info.plist"),
            ("LD_RUNPATH_SEARCH_PATHS", '(\n\t\t\t\t\t"$(inherited)",\n\t\t\t\t\t"@executable_path/Frameworks",\n\t\t\t\t)'),
            ("MARKETING_VERSION", self.marketing_version),
            ("PRODUCT_BUNDLE_IDENTIFIER", self.bundle_id),
            ("PRODUCT_NAME", '"$(TARGET_NAME)"'),
            ("SWIFT_EMIT_LOC_STRINGS", "YES"),
            ("TARGETED_DEVICE_FAMILY", '"1,2"'),
        ]
        target_debug = target_common + [
            ("CODE_SIGNING_ALLOWED", "NO"),
            ("CODE_SIGNING_REQUIRED", "NO"),
            ("CODE_SIGN_IDENTITY", '""'),
        ]
        target_release = target_common

        config_ids = {}
        for scope, name, settings in (
            ("project", "Debug", project_debug),
            ("project", "Release", project_release),
            ("target", "Debug", target_debug),
            ("target", "Release", target_release),
        ):
            cid = uid("config", scope, name)
            config_ids[(scope, name)] = cid
            body = "\n".join(f'\t\t\t\t{k} = {v};' for k, v in settings)
            self.objects.append(
                f'\t\t{cid} /* {name} */ = {{\n'
                f'\t\t\tisa = XCBuildConfiguration;\n'
                f'\t\t\tbuildSettings = {{\n{body}\n\t\t\t}};\n'
                f'\t\t\tname = {name};\n'
                '\t\t};'
            )

        for scope, list_id in (("project", project_config_list), ("target", target_config_list)):
            debug_id = config_ids[(scope, "Debug")]
            release_id = config_ids[(scope, "Release")]
            kind = "PBXProject" if scope == "project" else "PBXNativeTarget"
            self.objects.append(
                f'\t\t{list_id} /* Build configuration list for {kind} "{self.project_name}" */ = {{\n'
                f'\t\t\tisa = XCConfigurationList;\n'
                f'\t\t\tbuildConfigurations = (\n'
                f'\t\t\t\t{debug_id} /* Debug */,\n'
                f'\t\t\t\t{release_id} /* Release */,\n'
                f'\t\t\t);\n'
                f'\t\t\tdefaultConfigurationIsVisible = 0;\n'
                f'\t\t\tdefaultConfigurationName = Release;\n'
                '\t\t};'
            )

        body = "\n".join(self.objects)
        return (
            "// !$*UTF8*$!\n"
            "{\n"
            "\tarchiveVersion = 1;\n"
            "\tclasses = {\n\t};\n"
            "\tobjectVersion = 60;\n"
            "\tobjects = {\n"
            f"{body}\n"
            "\t};\n"
            f"\trootObject = {project_id} /* Project object */;\n"
            "}\n"
        )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--project-name", default="IvecAI")
    parser.add_argument("--bundle-id", default="com.ivec.ai")
    parser.add_argument("--team-id", default="CC6VQMAF97")
    parser.add_argument("--deployment-target", default="17.0")
    parser.add_argument("--marketing-version", default="0.1.0")
    parser.add_argument("--build-version", default="1")
    args = parser.parse_args()

    root = Path(__file__).resolve().parent.parent
    project = Project(
        root, args.project_name, args.bundle_id, args.team_id,
        args.deployment_target, args.marketing_version, args.build_version,
    )
    content = project.generate()

    out_dir = root / f"{args.project_name}.xcodeproj"
    out_dir.mkdir(parents=True, exist_ok=True)
    (out_dir / "project.pbxproj").write_text(content, encoding="utf-8")

    # Xcode 识别工程所需的最小 xcscheme（共享 scheme，方便命令行 -scheme 选中）
    schemes_dir = out_dir / "xcshareddata" / "xcschemes"
    schemes_dir.mkdir(parents=True, exist_ok=True)
    (schemes_dir / f"{args.project_name}.xcscheme").write_text(
        SCHEME_TEMPLATE.format(
            name=args.project_name,
            target=uid("target", args.project_name),
            project=uid("project", args.project_name),
            product=f"{args.project_name}.app",
        ),
        encoding="utf-8",
    )

    print(f"generated {out_dir}/project.pbxproj")
    return 0


SCHEME_TEMPLATE = """<?xml version="1.0" encoding="UTF-8"?>
<Scheme LastUpgradeVersion = "1600" version = "1.7">
   <BuildAction parallelizeBuildables = "YES" buildImplicitDependencies = "YES">
      <BuildActionEntries>
         <BuildActionEntry buildForTesting = "YES" buildForRunning = "YES" buildForProfiling = "YES" buildForArchiving = "YES" buildForAnalyzing = "YES">
            <BuildableReference
               BuildableIdentifier = "primary"
               BlueprintIdentifier = "{target}"
               BuildableName = "{product}"
               BlueprintName = "{name}"
               ReferencedContainer = "container:{name}.xcodeproj">
            </BuildableReference>
         </BuildActionEntry>
      </BuildActionEntries>
   </BuildAction>
   <TestAction buildConfiguration = "Debug" selectedDebuggerIdentifier = "Xcode.DebuggerFoundation.Debugger.LLDB" selectedLauncherIdentifier = "Xcode.DebuggerFoundation.Launcher.LLDB" shouldUseLaunchSchemeArgsEnv = "YES">
      <Testables>
      </Testables>
   </TestAction>
   <LaunchAction buildConfiguration = "Debug" selectedDebuggerIdentifier = "Xcode.DebuggerFoundation.Debugger.LLDB" selectedLauncherIdentifier = "Xcode.DebuggerFoundation.Launcher.LLDB" launchStyle = "0" useCustomWorkingDirectory = "NO" ignoresPersistentStateOnLaunch = "NO" debugDocumentVersioning = "YES" debugServiceExtension = "internal" allowLocationSimulation = "YES">
      <BuildableProductRunnable runnableDebuggingMode = "0">
         <BuildableReference
            BuildableIdentifier = "primary"
            BlueprintIdentifier = "{target}"
            BuildableName = "{product}"
            BlueprintName = "{name}"
            ReferencedContainer = "container:{name}.xcodeproj">
         </BuildableReference>
      </BuildableProductRunnable>
   </LaunchAction>
   <ProfileAction buildConfiguration = "Release" shouldUseLaunchSchemeArgsEnv = "YES" savedToolIdentifier = "" useCustomWorkingDirectory = "NO" debugDocumentVersioning = "YES">
      <BuildableProductRunnable runnableDebuggingMode = "0">
         <BuildableReference
            BuildableIdentifier = "primary"
            BlueprintIdentifier = "{target}"
            BuildableName = "{product}"
            BlueprintName = "{name}"
            ReferencedContainer = "container:{name}.xcodeproj">
         </BuildableReference>
      </BuildableProductRunnable>
   </ProfileAction>
   <AnalyzeAction buildConfiguration = "Debug">
   </AnalyzeAction>
   <ArchiveAction buildConfiguration = "Release" revealArchiveInOrganizer = "YES">
   </ArchiveAction>
</Scheme>
"""


if __name__ == "__main__":
    sys.exit(main())
